package service

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dentalv1 "dental-server/gen/dental/v1"
)

// Reviewing payments reported in the desktop app, and the accounts owners pay to.

func (s *AdminService) ListPayments(ctx context.Context, req *dentalv1.ListPaymentsRequest) (*dentalv1.ListPaymentsResponse, error) {
	limit := int32(req.GetLimit())
	if limit == 0 {
		limit = 100
	}
	status := paymentStatusNames[req.GetStatus()] // "" for all

	accounts, err := s.paymentAccounts(ctx)
	if err != nil {
		return nil, err
	}

	// Pending payments are a queue: oldest first. Otherwise newest first.
	rows, err := s.d.DB.Query(ctx, `SELECT`+paymentFields+`,
		u.username, u.full_name, coalesce(u.identity, ''), u.phone
		FROM payments p
		JOIN plans pl ON pl.id = p.plan_id
		JOIN users u ON u.id = p.user_id
		WHERE $1 = '' OR p.status = $1
		ORDER BY CASE WHEN $1 = 'pending' THEN p.created_at END, p.created_at DESC
		LIMIT $2`, status, limit)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	res := &dentalv1.ListPaymentsResponse{}
	for rows.Next() {
		r := &dentalv1.PaymentReview{User: &dentalv1.User{}, Identity: &dentalv1.PersonIdentity{}}
		payment, err := scanPayment(rows, &r.User.Username, &r.User.FullName, &r.Identity.Number, &r.Phone)
		if err != nil {
			return nil, internal(err)
		}
		r.Payment, r.User.Id = payment, payment.GetUserId()
		if t := payment.GetTransfer(); t != nil {
			id, _ := toUUID(t.GetPaymentAccountId())
			r.Account = accounts[id]
		}
		res.Payments = append(res.Payments, r)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// paymentAccounts returns every payment account by ID (there are only a few).
func (s *AdminService) paymentAccounts(ctx context.Context) (map[pgtype.UUID]*dentalv1.PaymentAccount, error) {
	rows, err := s.d.DB.Query(ctx, `SELECT `+paymentAccountFields+` FROM payment_accounts`)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	accounts := map[pgtype.UUID]*dentalv1.PaymentAccount{}
	for rows.Next() {
		a, err := scanPaymentAccount(rows)
		if err != nil {
			return nil, internal(err)
		}
		id, _ := toUUID(a.GetId())
		accounts[id] = a
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return accounts, nil
}

func (s *AdminService) ApprovePayment(ctx context.Context, req *dentalv1.ApprovePaymentRequest) (*dentalv1.ApprovePaymentResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	now := s.d.Now()
	adminID := adminSessionFrom(ctx).AdminID

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// The payment, then its owner, locked so the period cannot change meanwhile (as in SellSubscription).
	var userID pgtype.UUID
	var mode, status string
	var p purchase
	err = tx.QueryRow(ctx, `
		SELECT user_id, plan_id, max_clinics, max_devices_per_clinic, duration_days, price_rials, mode, status
		FROM payments WHERE id = $1 FOR UPDATE`, id,
	).Scan(&userID, &p.PlanID, &p.MaxClinics, &p.MaxDevicesPerClinic, &p.DurationDays, &p.PriceRials, &mode, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonPaymentNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	if status != paymentStatusNames[dentalv1.PaymentStatus_PAYMENT_STATUS_PENDING] {
		return nil, fail(connect.CodeFailedPrecondition, reasonPaymentNotPending)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
		return nil, internal(err)
	}

	// The limits and price were fixed when reported; the period starts now or when the paid time ends.
	if err := p.schedule(ctx, tx, userID, purchaseModeFromName(mode), now, true); err != nil {
		return nil, err
	}
	sub, err := insertSubscription(ctx, tx, userID, p, req.GetNote(), adminID, now)
	if err != nil {
		return nil, err
	}
	subscriptionID, _ := toUUID(sub.GetId())
	if _, err := tx.Exec(ctx, `
		UPDATE payments SET status = 'approved', subscription_id = $2, reviewed_by = $3, reviewed_at = $4
		WHERE id = $1`, id, subscriptionID, adminID, now); err != nil {
		return nil, internal(err)
	}
	payment, err := scanPayment(tx.QueryRow(ctx, `SELECT`+paymentColumns+` WHERE p.id = $1`, id))
	if err != nil {
		return nil, internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.ApprovePaymentResponse{Payment: payment, Subscription: sub}, nil
}

func (s *AdminService) RejectPayment(ctx context.Context, req *dentalv1.RejectPaymentRequest) (*dentalv1.RejectPaymentResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	tag, err := s.d.DB.Exec(ctx, `
		UPDATE payments SET status = 'rejected', rejection_reason = $2, reviewed_by = $3, reviewed_at = $4
		WHERE id = $1 AND status = 'pending'`, id, req.GetReason(), adminSessionFrom(ctx).AdminID, s.d.Now())
	if err != nil {
		return nil, internal(err)
	}
	payment, err := scanPayment(s.d.DB.QueryRow(ctx, `SELECT`+paymentColumns+` WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonPaymentNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fail(connect.CodeFailedPrecondition, reasonPaymentNotPending)
	}
	return &dentalv1.RejectPaymentResponse{Payment: payment}, nil
}

func (s *AdminService) ListPaymentAccounts(ctx context.Context, _ *dentalv1.ListPaymentAccountsRequest) (*dentalv1.ListPaymentAccountsResponse, error) {
	rows, err := s.d.DB.Query(ctx, `SELECT `+paymentAccountFields+` FROM payment_accounts ORDER BY active DESC, position, created_at, id`)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	res := &dentalv1.ListPaymentAccountsResponse{}
	for rows.Next() {
		a, err := scanPaymentAccount(rows)
		if err != nil {
			return nil, internal(err)
		}
		res.Accounts = append(res.Accounts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

func (s *AdminService) CreatePaymentAccount(ctx context.Context, req *dentalv1.CreatePaymentAccountRequest) (*dentalv1.CreatePaymentAccountResponse, error) {
	a, err := scanPaymentAccount(s.d.DB.QueryRow(ctx, `
		INSERT INTO payment_accounts (bank_name, holder_name, card_number, iban, position)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+paymentAccountFields,
		req.GetBankName(), req.GetHolderName(), req.GetCardNumber(), req.GetIban(), req.GetPosition()))
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.CreatePaymentAccountResponse{Account: a}, nil
}

func (s *AdminService) UpdatePaymentAccount(ctx context.Context, req *dentalv1.UpdatePaymentAccountRequest) (*dentalv1.UpdatePaymentAccountResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	a, err := scanPaymentAccount(s.d.DB.QueryRow(ctx, `
		UPDATE payment_accounts SET bank_name = $2, holder_name = $3, card_number = $4, iban = $5,
		                            position = $6, active = $7, updated_at = now()
		WHERE id = $1
		RETURNING `+paymentAccountFields,
		id, req.GetBankName(), req.GetHolderName(), req.GetCardNumber(), req.GetIban(), req.GetPosition(), req.GetActive()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonPaymentAccountNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.UpdatePaymentAccountResponse{Account: a}, nil
}
