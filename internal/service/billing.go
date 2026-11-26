package service

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

// BillingService lets clinic owners buy subscriptions. Every call needs a session of
// the owner of the session's clinic; billing-only sessions work here (see SessionInterceptor).
type BillingService struct {
	d *Deps
}

var _ dentalv1connect.BillingServiceHandler = (*BillingService)(nil)

func NewBillingService(d *Deps) *BillingService {
	return &BillingService{d: d}
}

// ownerOf returns the session's user, who must own the session's clinic.
func ownerOf(ctx context.Context) (pgtype.UUID, error) {
	s := sessionFrom(ctx)
	if !s.IsOwner {
		return s.UserID, fail(connect.CodePermissionDenied, reasonNotClinicOwner)
	}
	return s.UserID, nil
}

func (s *BillingService) ListPlans(ctx context.Context, _ *dentalv1.BillingServiceListPlansRequest) (*dentalv1.BillingServiceListPlansResponse, error) {
	ownerID, err := ownerOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.DB.Query(ctx, `
		SELECT `+planColumns+`, `+purchaseCountSQL+`
		FROM plans p WHERE active ORDER BY price_rials, name, id`, ownerID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	res := &dentalv1.BillingServiceListPlansResponse{}
	for rows.Next() {
		var count int64
		p, err := scanPlan(rows, &count)
		if err != nil {
			return nil, internal(err)
		}
		res.Plans = append(res.Plans, p)
		if p.GetMaxPurchasesPerOwner() > 0 {
			res.Purchases = append(res.Purchases, &dentalv1.PlanPurchases{PlanId: p.GetId(), Count: uint32(count)})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

func (s *BillingService) GetBillingStatus(ctx context.Context, _ *dentalv1.GetBillingStatusRequest) (*dentalv1.GetBillingStatusResponse, error) {
	ownerID, err := ownerOf(ctx)
	if err != nil {
		return nil, err
	}
	now := s.d.Now()
	res := &dentalv1.GetBillingStatusResponse{}

	// Subscriptions not over yet: the one in force, then the ones bought after it.
	subs, err := s.d.DB.Query(ctx, `SELECT`+subscriptionColumns+`
		WHERE s.user_id = $1 AND s.canceled_at IS NULL AND s.ends_at > $2 ORDER BY s.starts_at`, ownerID, now)
	if err != nil {
		return nil, internal(err)
	}
	defer subs.Close()
	for subs.Next() {
		sub, err := scanSubscription(subs)
		if err != nil {
			return nil, internal(err)
		}
		sub.Note = "" // the administrator's, not the owner's
		if res.Current == nil && !sub.GetStartsAt().AsTime().After(now) {
			res.Current = sub
		} else {
			res.Upcoming = append(res.Upcoming, sub)
		}
	}
	if err := subs.Err(); err != nil {
		return nil, internal(err)
	}

	paid, ok, err := paidTimeAt(ctx, s.d.DB, ownerID, now)
	if err != nil {
		return nil, internal(err)
	}
	if ok {
		res.PaidUntil = timestamppb.New(paid.EndsAt)
	}

	if res.Clinics, err = clinicUsage(ctx, s.d.DB, ownerID, paid, ok); err != nil {
		return nil, err
	}

	accounts, err := s.d.DB.Query(ctx, `SELECT `+paymentAccountFields+` FROM payment_accounts WHERE active ORDER BY position, created_at, id`)
	if err != nil {
		return nil, internal(err)
	}
	defer accounts.Close()
	for accounts.Next() {
		a, err := scanPaymentAccount(accounts)
		if err != nil {
			return nil, internal(err)
		}
		res.PayTo = append(res.PayTo, a)
	}
	if err := accounts.Err(); err != nil {
		return nil, internal(err)
	}

	pending, err := scanPayment(s.d.DB.QueryRow(ctx, `SELECT`+paymentColumns+`
		WHERE p.user_id = $1 AND p.status = 'pending'`, ownerID))
	switch {
	case err == nil:
		res.PendingPayment = pending
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, internal(err)
	}
	return res, nil
}

// clinicUsage lists the owner's clinics, oldest first, and whether the subscription
// in force (paid; ok = false if none) covers each.
func clinicUsage(ctx context.Context, q querier, ownerID pgtype.UUID, paid paidTime, ok bool) ([]*dentalv1.ClinicUsage, error) {
	rows, err := q.Query(ctx, `
		SELECT c.id, c.name, c.license_number, c.city,
		       (SELECT count(*) FROM installations i WHERE i.clinic_id = c.id AND i.revoked_at IS NULL)
		FROM clinics c WHERE c.owner_id = $1 ORDER BY c.created_at, c.id`, ownerID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	var clinics []*dentalv1.ClinicUsage
	for rows.Next() {
		var id pgtype.UUID
		var installations int64
		c := &dentalv1.ClinicUsage{}
		if err := rows.Scan(&id, &c.Name, &c.LicenseNumber, &c.City, &installations); err != nil {
			return nil, internal(err)
		}
		c.Id, c.InstallationCount = uuidBytes(id), uint32(installations)
		c.Covered = ok && len(clinics) < int(paid.MaxClinics)
		clinics = append(clinics, c)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return clinics, nil
}

func (s *BillingService) QuotePayment(ctx context.Context, req *dentalv1.QuotePaymentRequest) (*dentalv1.QuotePaymentResponse, error) {
	ownerID, err := ownerOf(ctx)
	if err != nil {
		return nil, err
	}
	planID, ok := toUUID(req.GetPlanId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	p, err := quotePurchase(ctx, s.d.DB, ownerID, planID, req.GetMode(), s.d.Now())
	if err != nil {
		return nil, err
	}
	if err := checkPurchaseLimit(ctx, s.d.DB, ownerID, planID); err != nil {
		return nil, err
	}
	return &dentalv1.QuotePaymentResponse{Quote: &dentalv1.PaymentQuote{
		PriceRials:  p.PriceRials,
		CreditRials: p.CreditRials,
		AmountRials: p.PriceRials - p.CreditRials,
		StartsAt:    timestamppb.New(p.StartsAt),
		EndsAt:      timestamppb.New(p.EndsAt()),
	}}, nil
}

func (s *BillingService) CreatePayment(ctx context.Context, req *dentalv1.CreatePaymentRequest) (*dentalv1.CreatePaymentResponse, error) {
	ownerID, err := ownerOf(ctx)
	if err != nil {
		return nil, err
	}
	planID, ok := toUUID(req.GetPlanId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	now := s.d.Now()

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// The owner, locked so the quote cannot change before the payment is saved.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, ownerID); err != nil {
		return nil, internal(err)
	}
	p, err := quotePurchase(ctx, tx, ownerID, planID, req.GetMode(), now)
	if err != nil {
		return nil, err
	}
	if err := checkPurchaseLimit(ctx, tx, ownerID, planID); err != nil {
		return nil, err
	}
	free := req.GetMethod() == dentalv1.PaymentMethod_PAYMENT_METHOD_FREE
	switch amount := p.PriceRials - p.CreditRials; {
	case free && amount != 0:
		return nil, fail(connect.CodeFailedPrecondition, reasonPaymentRequired)
	case !free && amount == 0:
		return nil, fail(connect.CodeFailedPrecondition, reasonPaymentNotRequired)
	case free:
		id, _, err := activateFree(ctx, tx, ownerID, req.GetMode(), p, now)
		if err != nil {
			return nil, err
		}
		return s.commitPayment(ctx, tx, id)
	}

	transfer := req.GetTransfer()
	accountID, ok := toUUID(transfer.GetPaymentAccountId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT active FROM payment_accounts WHERE id = $1`, accountID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !active) {
		return nil, fail(connect.CodeNotFound, reasonPaymentAccountNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}

	var id pgtype.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO payments (user_id, plan_id, max_clinics, max_devices_per_clinic, duration_days, mode, method,
		                      price_rials, credit_rials, amount_rials, payment_account_id, reference, paid_at,
		                      payer_card_last4, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15) RETURNING id`,
		ownerID, planID, p.MaxClinics, p.MaxDevicesPerClinic, p.DurationDays,
		purchaseModeNames[req.GetMode()], paymentMethodNames[req.GetMethod()],
		p.PriceRials, p.CreditRials, p.PriceRials-p.CreditRials, accountID, transfer.GetReference(),
		transfer.GetPaidAt().AsTime(), transfer.GetPayerCardLast4(), now,
	).Scan(&id)
	if uniqueViolation(err, "payments_one_pending_key") {
		return nil, fail(connect.CodeFailedPrecondition, reasonPaymentPending)
	}
	if err != nil {
		return nil, internal(err)
	}
	return s.commitPayment(ctx, tx, id)
}

// commitPayment commits the transaction that saved a payment and returns the payment.
func (s *BillingService) commitPayment(ctx context.Context, tx pgx.Tx, id pgtype.UUID) (*dentalv1.CreatePaymentResponse, error) {
	payment, err := scanPayment(tx.QueryRow(ctx, `SELECT`+paymentColumns+` WHERE p.id = $1`, id))
	if err != nil {
		return nil, internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.CreatePaymentResponse{Payment: payment}, nil
}

// purchaseCountSQL counts an owner's ($1) purchases of a plan (p.id): subscriptions
// not canceled, however they were sold, and a payment waiting for review.
const purchaseCountSQL = `(
	(SELECT count(*) FROM subscriptions s WHERE s.user_id = $1 AND s.plan_id = p.id AND s.canceled_at IS NULL) +
	(SELECT count(*) FROM payments y WHERE y.user_id = $1 AND y.plan_id = p.id AND y.status = 'pending'))`

// checkPurchaseLimit fails with purchase_limit_reached when the owner already bought
// the plan as many times as it allows (Plan.max_purchases_per_owner; 0 is no limit).
func checkPurchaseLimit(ctx context.Context, q querier, ownerID, planID pgtype.UUID) error {
	var reached bool
	if err := q.QueryRow(ctx, `
		SELECT p.max_purchases_per_owner > 0 AND `+purchaseCountSQL+` >= p.max_purchases_per_owner
		FROM plans p WHERE p.id = $2`, ownerID, planID,
	).Scan(&reached); err != nil {
		return internal(err)
	}
	if reached {
		return fail(connect.CodeFailedPrecondition, reasonPurchaseLimitReached)
	}
	return nil
}

func (s *BillingService) ListMyPayments(ctx context.Context, _ *dentalv1.ListMyPaymentsRequest) (*dentalv1.ListMyPaymentsResponse, error) {
	ownerID, err := ownerOf(ctx)
	if err != nil {
		return nil, err
	}
	payments, err := paymentsOf(ctx, s.d.DB, ownerID)
	if err != nil {
		return nil, err
	}
	return &dentalv1.ListMyPaymentsResponse{Payments: payments}, nil
}

func (s *BillingService) CancelPayment(ctx context.Context, req *dentalv1.CancelPaymentRequest) (*dentalv1.CancelPaymentResponse, error) {
	ownerID, err := ownerOf(ctx)
	if err != nil {
		return nil, err
	}
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	tag, err := s.d.DB.Exec(ctx, `
		UPDATE payments SET status = 'canceled', reviewed_at = $3
		WHERE id = $1 AND user_id = $2 AND status = 'pending'`, id, ownerID, s.d.Now())
	if err != nil {
		return nil, internal(err)
	}
	// Someone else's payment is reported as not found.
	payment, err := scanPayment(s.d.DB.QueryRow(ctx, `SELECT`+paymentColumns+`
		WHERE p.id = $1 AND p.user_id = $2`, id, ownerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonPaymentNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fail(connect.CodeFailedPrecondition, reasonPaymentNotPending)
	}
	return &dentalv1.CancelPaymentResponse{Payment: payment}, nil
}
