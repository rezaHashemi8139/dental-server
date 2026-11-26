package service

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
)

// Buying subscriptions, shared by BillingService (owners) and AdminService
// (selling and approving payments). See proto/dental/v1/subscription.proto.

// paidTimeEnd returns when the paid time the user already bought ends, or now if it
// has ended: where a subscription bought now (PURCHASE_MODE_EXTEND) starts.
func paidTimeEnd(ctx context.Context, q querier, userID pgtype.UUID, now time.Time) (time.Time, error) {
	var end time.Time
	err := q.QueryRow(ctx, `
		SELECT greatest($2::timestamptz, max(ends_at)) FROM subscriptions
		WHERE user_id = $1 AND canceled_at IS NULL`, userID, now,
	).Scan(&end)
	return end, err
}

// upgradeCredit returns the unused value of the user's subscription in force at
// now (0 if none), for PURCHASE_MODE_UPGRADE_NOW. Fails with renewal_queued when a
// subscription starts later: an upgrade would have to move it.
func upgradeCredit(ctx context.Context, q querier, userID pgtype.UUID, now time.Time) (int64, error) {
	var queued bool
	var credit int64
	err := q.QueryRow(ctx, `
		SELECT
			EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1 AND canceled_at IS NULL AND starts_at > $2),
			coalesce((
				SELECT floor(price_rials * extract(epoch FROM ends_at - $2) / extract(epoch FROM ends_at - starts_at))::bigint
				FROM subscriptions
				WHERE user_id = $1 AND canceled_at IS NULL AND starts_at <= $2 AND ends_at > $2
				ORDER BY starts_at LIMIT 1), 0)`, userID, now,
	).Scan(&queued, &credit)
	if err != nil {
		return 0, internal(err)
	}
	if queued {
		return 0, fail(connect.CodeFailedPrecondition, reasonRenewalQueued)
	}
	return credit, nil
}

// purchase is a subscription being bought: what it gives and what it costs.
type purchase struct {
	PlanID              pgtype.UUID
	MaxClinics          int32
	MaxDevicesPerClinic int32
	DurationDays        int32
	PriceRials          int64
	// The unused value of the subscription in force, for PURCHASE_MODE_UPGRADE_NOW; at most PriceRials.
	CreditRials int64
	StartsAt    time.Time
}

func (p purchase) EndsAt() time.Time {
	return p.StartsAt.Add(time.Duration(p.DurationDays) * 24 * time.Hour)
}

// quotePurchase prices an active plan for the user as if bought now. Fails with
// plan_not_found, plan_inactive or (upgrades) renewal_queued.
func quotePurchase(ctx context.Context, q querier, userID, planID pgtype.UUID, mode dentalv1.PurchaseMode, now time.Time) (purchase, error) {
	p := purchase{PlanID: planID}
	var active bool
	err := q.QueryRow(ctx, `
		SELECT max_clinics, max_devices_per_clinic, duration_days, price_rials, active FROM plans WHERE id = $1`, planID,
	).Scan(&p.MaxClinics, &p.MaxDevicesPerClinic, &p.DurationDays, &p.PriceRials, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, fail(connect.CodeNotFound, reasonPlanNotFound)
	}
	if err != nil {
		return p, internal(err)
	}
	if !active {
		return p, fail(connect.CodeFailedPrecondition, reasonPlanInactive)
	}
	if err := p.schedule(ctx, q, userID, mode, now, false); err != nil {
		return p, err
	}
	return p, nil
}

// schedule sets when the purchase starts if bought now and, for a quoted upgrade,
// its credit. With apply (an approval), an upgrade instead ends the subscription in
// force now and keeps the credit already quoted.
func (p *purchase) schedule(ctx context.Context, q querier, userID pgtype.UUID, mode dentalv1.PurchaseMode, now time.Time, apply bool) error {
	if mode != dentalv1.PurchaseMode_PURCHASE_MODE_UPGRADE_NOW {
		start, err := paidTimeEnd(ctx, q, userID, now)
		if err != nil {
			return internal(err)
		}
		p.StartsAt = start
		return nil
	}
	credit, err := upgradeCredit(ctx, q, userID, now)
	if err != nil {
		return err
	}
	if !apply {
		p.CreditRials = min(credit, p.PriceRials)
	} else if _, err := q.Exec(ctx, `
		UPDATE subscriptions SET ends_at = $2
		WHERE user_id = $1 AND canceled_at IS NULL AND starts_at < $2 AND ends_at > $2`, userID, now); err != nil {
		return internal(err)
	}
	p.StartsAt = now
	return nil
}

// insertSubscription adds the subscription a purchase buys and returns it.
// soldBy is the administrator who sold it or approved its payment.
func insertSubscription(ctx context.Context, q querier, userID pgtype.UUID, p purchase, note string, soldBy pgtype.UUID, now time.Time) (*dentalv1.Subscription, error) {
	var id pgtype.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, max_clinics, max_devices_per_clinic, price_rials,
		                           starts_at, ends_at, note, sold_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		userID, p.PlanID, p.MaxClinics, p.MaxDevicesPerClinic, p.PriceRials, p.StartsAt, p.EndsAt(), note, soldBy, now,
	).Scan(&id); err != nil {
		return nil, internal(err)
	}
	sub, err := scanSubscription(q.QueryRow(ctx, `SELECT`+subscriptionColumns+` WHERE s.id = $1`, id))
	if err != nil {
		return nil, internal(err)
	}
	return sub, nil
}

var purchaseModeNames = map[dentalv1.PurchaseMode]string{
	dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND:      "extend",
	dentalv1.PurchaseMode_PURCHASE_MODE_UPGRADE_NOW: "upgrade_now",
}

var paymentMethodNames = map[dentalv1.PaymentMethod]string{
	dentalv1.PaymentMethod_PAYMENT_METHOD_CARD_TO_CARD:  "card_to_card",
	dentalv1.PaymentMethod_PAYMENT_METHOD_BANK_TRANSFER: "bank_transfer",
	dentalv1.PaymentMethod_PAYMENT_METHOD_FREE:          "free",
}

var paymentStatusNames = map[dentalv1.PaymentStatus]string{
	dentalv1.PaymentStatus_PAYMENT_STATUS_PENDING:  "pending",
	dentalv1.PaymentStatus_PAYMENT_STATUS_APPROVED: "approved",
	dentalv1.PaymentStatus_PAYMENT_STATUS_REJECTED: "rejected",
	dentalv1.PaymentStatus_PAYMENT_STATUS_CANCELED: "canceled",
}

func purchaseModeFromName(name string) dentalv1.PurchaseMode {
	for m, n := range purchaseModeNames {
		if n == name {
			return m
		}
	}
	return dentalv1.PurchaseMode_PURCHASE_MODE_UNSPECIFIED
}

func paymentMethodFromName(name string) dentalv1.PaymentMethod {
	for m, n := range paymentMethodNames {
		if n == name {
			return m
		}
	}
	return dentalv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED
}

func paymentStatusFromName(name string) dentalv1.PaymentStatus {
	for s, n := range paymentStatusNames {
		if n == name {
			return s
		}
	}
	return dentalv1.PaymentStatus_PAYMENT_STATUS_UNSPECIFIED
}

// paymentFields are the columns scanPayment reads; paymentColumns adds the tables they come from.
const paymentFields = `
	p.id, p.user_id, p.plan_id, pl.name, p.max_clinics, p.max_devices_per_clinic, p.duration_days,
	p.mode, p.method, p.status, p.price_rials, p.credit_rials, p.amount_rials,
	p.payment_account_id, p.reference, p.paid_at, p.payer_card_last4,
	p.rejection_reason, p.subscription_id, p.created_at, p.reviewed_at`

const paymentColumns = paymentFields + `
	FROM payments p
	JOIN plans pl ON pl.id = p.plan_id`

// scanPayment scans paymentColumns, then any extra destinations selected after them.
func scanPayment(row pgx.Row, extra ...any) (*dentalv1.Payment, error) {
	var id, userID, planID, accountID, subscriptionID pgtype.UUID
	var maxClinics, maxDevices, days int32
	var mode, method, status, reference, last4 string
	var paidAt, reviewedAt pgtype.Timestamptz
	var createdAt time.Time
	p := &dentalv1.Payment{}
	dest := []any{&id, &userID, &planID, &p.PlanName, &maxClinics, &maxDevices, &days,
		&mode, &method, &status, &p.PriceRials, &p.CreditRials, &p.AmountRials,
		&accountID, &reference, &paidAt, &last4,
		&p.RejectionReason, &subscriptionID, &createdAt, &reviewedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	p.Id, p.UserId, p.PlanId = uuidBytes(id), uuidBytes(userID), uuidBytes(planID)
	p.MaxClinics, p.MaxDevicesPerClinic, p.DurationDays = uint32(maxClinics), uint32(maxDevices), uint32(days)
	p.Mode, p.Method, p.Status = purchaseModeFromName(mode), paymentMethodFromName(method), paymentStatusFromName(status)
	if accountID.Valid {
		transfer := &dentalv1.ManualTransfer{PaymentAccountId: uuidBytes(accountID), Reference: reference, PayerCardLast4: last4}
		if paidAt.Valid {
			transfer.PaidAt = timestamppb.New(paidAt.Time)
		}
		p.Details = &dentalv1.Payment_Transfer{Transfer: transfer}
	}
	if subscriptionID.Valid {
		p.SubscriptionId = uuidBytes(subscriptionID)
	}
	p.CreatedAt = timestamppb.New(createdAt)
	if reviewedAt.Valid {
		p.ReviewedAt = timestamppb.New(reviewedAt.Time)
	}
	return p, nil
}

// paymentAccountFields are the columns scanPaymentAccount reads.
const paymentAccountFields = `id, bank_name, holder_name, card_number, iban, active, position`

func scanPaymentAccount(row pgx.Row) (*dentalv1.PaymentAccount, error) {
	var id pgtype.UUID
	a := &dentalv1.PaymentAccount{}
	if err := row.Scan(&id, &a.BankName, &a.HolderName, &a.CardNumber, &a.Iban, &a.Active, &a.Position); err != nil {
		return nil, err
	}
	a.Id = uuidBytes(id)
	return a, nil
}

// paymentsOf returns a user's payments, newest first (at most 100).
func paymentsOf(ctx context.Context, q querier, userID pgtype.UUID) ([]*dentalv1.Payment, error) {
	rows, err := q.Query(ctx, `SELECT`+paymentColumns+` WHERE p.user_id = $1 ORDER BY p.created_at DESC LIMIT 100`, userID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var payments []*dentalv1.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, internal(err)
		}
		payments = append(payments, p)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return payments, nil
}

// activateFree buys a purchase with nothing to pay: the subscription is added at
// once, as ApprovePayment would, and the payment saved as approved by nobody.
// Like any other purchase, it waits while another payment is pending.
func activateFree(ctx context.Context, tx pgx.Tx, ownerID pgtype.UUID, mode dentalv1.PurchaseMode,
	p purchase, now time.Time) (pgtype.UUID, *dentalv1.Subscription, error) {
	var id pgtype.UUID
	var pending bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM payments WHERE user_id = $1 AND status = 'pending')`, ownerID,
	).Scan(&pending); err != nil {
		return id, nil, internal(err)
	}
	if pending {
		return id, nil, fail(connect.CodeFailedPrecondition, reasonPaymentPending)
	}

	if err := p.schedule(ctx, tx, ownerID, mode, now, true); err != nil {
		return id, nil, err
	}
	sub, err := insertSubscription(ctx, tx, ownerID, p, "", pgtype.UUID{}, now)
	if err != nil {
		return id, nil, err
	}
	subscriptionID, _ := toUUID(sub.GetId())
	if err := tx.QueryRow(ctx, `
		INSERT INTO payments (user_id, plan_id, max_clinics, max_devices_per_clinic, duration_days, mode, method,
		                      status, price_rials, credit_rials, amount_rials, subscription_id, reviewed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'approved', $8, $9, 0, $10, $11, $11) RETURNING id`,
		ownerID, p.PlanID, p.MaxClinics, p.MaxDevicesPerClinic, p.DurationDays,
		purchaseModeNames[mode], paymentMethodNames[dentalv1.PaymentMethod_PAYMENT_METHOD_FREE],
		p.PriceRials, p.CreditRials, subscriptionID, now,
	).Scan(&id); err != nil {
		return id, nil, internal(err)
	}
	return id, sub, nil
}

// grantFreePlan starts a free plan for an owner who just registered their first
// clinic and never had a subscription, without being asked: the oldest active free
// plan they may still buy (see Plan.max_purchases_per_owner). Returns nil if there
// is none, or the owner does not qualify.
func grantFreePlan(ctx context.Context, tx pgx.Tx, ownerID pgtype.UUID, now time.Time) (*dentalv1.Subscription, error) {
	var planID pgtype.UUID
	err := tx.QueryRow(ctx, `
		SELECT p.id FROM plans p
		WHERE p.active AND p.price_rials = 0
		  AND (p.max_purchases_per_owner = 0 OR `+purchaseCountSQL+` < p.max_purchases_per_owner)
		  AND (SELECT count(*) FROM clinics WHERE owner_id = $1) = 1
		  AND NOT EXISTS (SELECT 1 FROM subscriptions WHERE user_id = $1)
		ORDER BY p.created_at, p.id
		LIMIT 1`, ownerID,
	).Scan(&planID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, internal(err)
	}

	p, err := quotePurchase(ctx, tx, ownerID, planID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, now)
	if err != nil {
		return nil, err
	}
	_, sub, err := activateFree(ctx, tx, ownerID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, p, now)
	if err != nil {
		return nil, err
	}
	sub.Note = "" // for the owner, as in BillingService
	return sub, nil
}
