package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

// AdminService is the admin panel. It is served with AdminSessionInterceptor, not SessionInterceptor.
type AdminService struct {
	d *Deps
}

var _ dentalv1connect.AdminServiceHandler = (*AdminService)(nil)

func NewAdminService(d *Deps) *AdminService {
	return &AdminService{d: d}
}

func (s *AdminService) Login(ctx context.Context, req *dentalv1.AdminServiceLoginRequest) (*dentalv1.AdminServiceLoginResponse, error) {
	// Unknown administrators are checked against a dummy hash so timing does not reveal them.
	var id pgtype.UUID
	var passwordHash string
	var disabledAt pgtype.Timestamptz
	admin := &dentalv1.Admin{}
	err := s.d.DB.QueryRow(ctx, `
		SELECT id, username, full_name, password_hash, disabled_at
		FROM admins WHERE lower(username) = lower($1)`, req.GetUsername(),
	).Scan(&id, &admin.Username, &admin.FullName, &passwordHash, &disabledAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, internal(err)
	}
	if !secure.VerifyPassword(req.GetPassword(), passwordHash) {
		return nil, fail(connect.CodeUnauthenticated, reasonInvalidCredentials)
	}
	if disabledAt.Valid {
		return nil, fail(connect.CodePermissionDenied, reasonUserDisabled)
	}
	admin.Id = uuidBytes(id)

	// The session row is what Logout revokes; the JWT (cookie) names it and the
	// CSRF token (response) must come with it, so both are needed on every call.
	now := s.d.Now()
	csrfToken, csrfHash := secure.NewToken()
	expires := now.Add(s.d.SessionTTL)
	var sessionID pgtype.UUID
	if err := s.d.DB.QueryRow(ctx, `
		INSERT INTO admin_sessions (csrf_hash, admin_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4) RETURNING id`, csrfHash, id, now, expires,
	).Scan(&sessionID); err != nil {
		return nil, internal(err)
	}
	token, err := s.d.AdminTokens.Sign(secure.TokenClaims{
		Subject:   id.String(),
		SessionID: sessionID.String(),
		Audience:  adminTokenAudience,
		IssuedAt:  now.Unix(),
		ExpiresAt: expires.Unix(),
	})
	if err != nil {
		return nil, internal(err)
	}
	if err := setCookie(ctx, adminCookie(token, int(s.d.SessionTTL/time.Second))); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.AdminServiceLoginResponse{
		SessionExpiresAt: timestamppb.New(expires),
		Admin:            admin,
		CsrfToken:        csrfToken,
	}, nil
}

// Logout revokes the session, so its JWT and CSRF token stop working at once
// even though the JWT has not expired, and tells the browser to drop the cookie.
func (s *AdminService) Logout(ctx context.Context, _ *dentalv1.AdminServiceLogoutRequest) (*dentalv1.AdminServiceLogoutResponse, error) {
	session := adminSessionFrom(ctx)
	if _, err := s.d.DB.Exec(ctx, `UPDATE admin_sessions SET revoked_at = $2 WHERE id = $1`, session.ID, s.d.Now()); err != nil {
		return nil, internal(err)
	}
	if err := setCookie(ctx, adminCookie("", -1)); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.AdminServiceLogoutResponse{}, nil
}

func (s *AdminService) CreateAdmin(ctx context.Context, req *dentalv1.CreateAdminRequest) (*dentalv1.CreateAdminResponse, error) {
	admin, err := insertAdmin(ctx, s.d.DB, req, adminSessionFrom(ctx).AdminID)
	if err != nil {
		return nil, err
	}
	return &dentalv1.CreateAdminResponse{Admin: admin}, nil
}

// AddFirstAdmin creates an administrator outside the API, for `dental-server create-admin`.
// The request is checked against the same rules as AdminService.CreateAdmin; errors
// carry the same reasons.
func AddFirstAdmin(ctx context.Context, db *pgxpool.Pool, req *dentalv1.CreateAdminRequest) (*dentalv1.Admin, error) {
	if err := protovalidate.Validate(req); err != nil {
		if validationErr, ok := errors.AsType[*protovalidate.ValidationError](err); ok {
			return nil, fail(connect.CodeInvalidArgument, reasonFor(validationErr))
		}
		return nil, err
	}
	return insertAdmin(ctx, db, req, pgtype.UUID{})
}

// insertAdmin creates an administrator. createdBy is NULL for the command line.
func insertAdmin(ctx context.Context, q querier, req *dentalv1.CreateAdminRequest, createdBy pgtype.UUID) (*dentalv1.Admin, error) {
	var id pgtype.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO admins (username, password_hash, full_name, created_by)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		req.GetUsername(), secure.HashPassword(req.GetPassword()), req.GetFullName(), createdBy,
	).Scan(&id)
	if uniqueViolation(err, "admins_username_key") {
		return nil, fail(connect.CodeAlreadyExists, reasonUsernameTaken)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.Admin{Id: uuidBytes(id), Username: req.GetUsername(), FullName: req.GetFullName()}, nil
}

const planColumns = `id, name, max_clinics, max_devices_per_clinic, duration_days, price_rials, active, max_purchases_per_owner`

// scanPlan scans planColumns, then any extra destinations selected after them.
func scanPlan(row pgx.Row, extra ...any) (*dentalv1.Plan, error) {
	var id pgtype.UUID
	var maxClinics, maxDevices, days, maxPurchases int32
	p := &dentalv1.Plan{}
	dest := []any{&id, &p.Name, &maxClinics, &maxDevices, &days, &p.PriceRials, &p.Active, &maxPurchases}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	p.Id = uuidBytes(id)
	p.MaxClinics, p.MaxDevicesPerClinic, p.DurationDays = uint32(maxClinics), uint32(maxDevices), uint32(days)
	p.MaxPurchasesPerOwner = uint32(maxPurchases)
	return p, nil
}

func (s *AdminService) ListPlans(ctx context.Context, _ *dentalv1.ListPlansRequest) (*dentalv1.ListPlansResponse, error) {
	rows, err := s.d.DB.Query(ctx, `SELECT `+planColumns+` FROM plans ORDER BY active DESC, created_at`)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	res := &dentalv1.ListPlansResponse{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, internal(err)
		}
		res.Plans = append(res.Plans, p)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// Plan limits are checked by ValidateInterceptor (1–1000, 1–3650 days), so they fit in an integer column.

func (s *AdminService) CreatePlan(ctx context.Context, req *dentalv1.CreatePlanRequest) (*dentalv1.CreatePlanResponse, error) {
	plan, err := scanPlan(s.d.DB.QueryRow(ctx, `
		INSERT INTO plans (name, max_clinics, max_devices_per_clinic, duration_days, price_rials, max_purchases_per_owner)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+planColumns,
		req.GetName(), int32(req.GetMaxClinics()), int32(req.GetMaxDevicesPerClinic()),
		int32(req.GetDurationDays()), req.GetPriceRials(), int32(req.GetMaxPurchasesPerOwner())))
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.CreatePlanResponse{Plan: plan}, nil
}

func (s *AdminService) UpdatePlan(ctx context.Context, req *dentalv1.UpdatePlanRequest) (*dentalv1.UpdatePlanResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	plan, err := scanPlan(s.d.DB.QueryRow(ctx, `
		UPDATE plans SET name = $2, max_clinics = $3, max_devices_per_clinic = $4, duration_days = $5,
		                 price_rials = $6, active = $7, max_purchases_per_owner = $8, updated_at = now()
		WHERE id = $1 RETURNING `+planColumns,
		id, req.GetName(), int32(req.GetMaxClinics()), int32(req.GetMaxDevicesPerClinic()),
		int32(req.GetDurationDays()), req.GetPriceRials(), req.GetActive(), int32(req.GetMaxPurchasesPerOwner())))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonPlanNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.UpdatePlanResponse{Plan: plan}, nil
}

const subscriptionFields = `
	s.id, s.user_id, s.plan_id, p.name, s.max_clinics, s.max_devices_per_clinic, s.price_rials,
	s.starts_at, s.ends_at, s.canceled_at, s.note`

const subscriptionColumns = subscriptionFields + `
	FROM subscriptions s
	JOIN plans p ON p.id = s.plan_id`

// scanSubscription scans subscriptionColumns, then any extra destinations selected after them.
func scanSubscription(row pgx.Row, extra ...any) (*dentalv1.Subscription, error) {
	var id, userID, planID pgtype.UUID
	var maxClinics, maxDevices int32
	var startsAt, endsAt time.Time
	var canceledAt pgtype.Timestamptz
	sub := &dentalv1.Subscription{}
	dest := []any{&id, &userID, &planID, &sub.PlanName, &maxClinics, &maxDevices, &sub.PriceRials,
		&startsAt, &endsAt, &canceledAt, &sub.Note}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	sub.Id, sub.UserId, sub.PlanId = uuidBytes(id), uuidBytes(userID), uuidBytes(planID)
	sub.MaxClinics, sub.MaxDevicesPerClinic = uint32(maxClinics), uint32(maxDevices)
	sub.StartsAt, sub.EndsAt = timestamppb.New(startsAt), timestamppb.New(endsAt)
	if canceledAt.Valid {
		sub.CanceledAt = timestamppb.New(canceledAt.Time)
	}
	return sub, nil
}

func (s *AdminService) GetUser(ctx context.Context, req *dentalv1.GetUserRequest) (*dentalv1.GetUserResponse, error) {
	var id pgtype.UUID
	var identity pgtype.Text
	var disabledAt pgtype.Timestamptz
	var createdAt time.Time
	res := &dentalv1.GetUserResponse{User: &dentalv1.User{}}
	err := s.d.DB.QueryRow(ctx, `
		SELECT id, username, full_name, identity, phone, disabled_at, created_at
		FROM users WHERE identity = $1`, req.GetIdentity().GetNumber(),
	).Scan(&id, &res.User.Username, &res.User.FullName, &identity, &res.Phone, &disabledAt, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	res.User.Id = uuidBytes(id)
	res.Identity = &dentalv1.PersonIdentity{Number: identity.String}
	res.Disabled = disabledAt.Valid
	res.CreatedAt = timestamppb.New(createdAt)

	clinics, err := s.d.DB.Query(ctx, `
		SELECT c.id, c.name, c.license_number, c.city, c.created_at,
		       (SELECT count(*) FROM installations i WHERE i.clinic_id = c.id AND i.revoked_at IS NULL)
		FROM clinics c WHERE c.owner_id = $1 ORDER BY c.created_at, c.id`, id)
	if err != nil {
		return nil, internal(err)
	}
	defer clinics.Close()
	for clinics.Next() {
		var clinicID pgtype.UUID
		var clinicCreated time.Time
		var installations int64
		c := &dentalv1.ClinicSummary{}
		if err := clinics.Scan(&clinicID, &c.Name, &c.LicenseNumber, &c.City, &clinicCreated, &installations); err != nil {
			return nil, internal(err)
		}
		c.Id, c.CreatedAt, c.InstallationCount = uuidBytes(clinicID), timestamppb.New(clinicCreated), uint32(installations)
		res.OwnedClinics = append(res.OwnedClinics, c)
	}
	if err := clinics.Err(); err != nil {
		return nil, internal(err)
	}

	subs, err := s.d.DB.Query(ctx, `SELECT`+subscriptionColumns+` WHERE s.user_id = $1 ORDER BY s.starts_at DESC`, id)
	if err != nil {
		return nil, internal(err)
	}
	defer subs.Close()
	for subs.Next() {
		sub, err := scanSubscription(subs)
		if err != nil {
			return nil, internal(err)
		}
		res.Subscriptions = append(res.Subscriptions, sub)
	}
	if err := subs.Err(); err != nil {
		return nil, internal(err)
	}

	if res.Payments, err = paymentsOf(ctx, s.d.DB, id); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *AdminService) SellSubscription(ctx context.Context, req *dentalv1.SellSubscriptionRequest) (*dentalv1.SellSubscriptionResponse, error) {
	userID, ok := toUUID(req.GetUserId())
	planID, ok2 := toUUID(req.GetPlanId())
	if !ok || !ok2 {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	now := s.d.Now()

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// The user, locked so two sales at once cannot both start at the same time.
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}

	// Starts when the paid time already bought ends, or now.
	p, err := quotePurchase(ctx, tx, userID, planID, dentalv1.PurchaseMode_PURCHASE_MODE_EXTEND, now)
	if err != nil {
		return nil, err
	}
	sub, err := insertSubscription(ctx, tx, userID, p, req.GetNote(), adminSessionFrom(ctx).AdminID, now)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.SellSubscriptionResponse{Subscription: sub}, nil
}

func (s *AdminService) CancelSubscription(ctx context.Context, req *dentalv1.CancelSubscriptionRequest) (*dentalv1.CancelSubscriptionResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	if _, err := s.d.DB.Exec(ctx, `
		UPDATE subscriptions SET canceled_at = $2, canceled_by = $3 WHERE id = $1 AND canceled_at IS NULL`,
		id, s.d.Now(), adminSessionFrom(ctx).AdminID); err != nil {
		return nil, internal(err)
	}

	sub, err := scanSubscription(s.d.DB.QueryRow(ctx, `SELECT`+subscriptionColumns+` WHERE s.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonSubscriptionNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.CancelSubscriptionResponse{Subscription: sub}, nil
}

var subscriptionStatusNames = map[dentalv1.SubscriptionStatus]string{
	dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE:   "active",
	dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UPCOMING: "upcoming",
	dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ENDED:    "ended",
	dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED: "canceled",
}

func subscriptionStatusFromName(name string) dentalv1.SubscriptionStatus {
	for s, n := range subscriptionStatusNames {
		if n == name {
			return s
		}
	}
	return dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UNSPECIFIED
}

// subscriptionStatusSQL is where a subscription stands at $1 (see SubscriptionStatus).
const subscriptionStatusSQL = `CASE
	WHEN s.canceled_at IS NOT NULL THEN 'canceled'
	WHEN $1 < s.starts_at THEN 'upcoming'
	WHEN $1 < s.ends_at THEN 'active'
	ELSE 'ended' END`

// ownerMatchesSQL is true when the owner u matches the search text in the
// parameter param (lower case, see searchText).
func ownerMatchesSQL(param string) string {
	return strings.NewReplacer("$q", param).Replace(`
		strpos(lower(u.full_name), $q) > 0 OR strpos(lower(u.username), $q) > 0
		OR strpos(lower(coalesce(u.identity, '')), $q) > 0 OR strpos(lower(u.phone), $q) > 0`)
}

// pageBounds returns the LIMIT and OFFSET of a page (from 1; 0 means 1) of size (0 means 20).
func pageBounds(page, size uint32) (limit, offset int64) {
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = 20
	}
	return int64(size), int64(page-1) * int64(size)
}

// searchText normalizes a list search: trimmed and lower case, as ownerMatchesSQL compares.
func searchText(query string) string {
	return strings.ToLower(strings.TrimSpace(query))
}

func (s *AdminService) ListSubscriptions(ctx context.Context, req *dentalv1.ListSubscriptionsRequest) (*dentalv1.ListSubscriptionsResponse, error) {
	now := s.d.Now()
	status := subscriptionStatusNames[req.GetStatus()] // "" for all
	query := searchText(req.GetQuery())
	limit, offset := pageBounds(req.GetPage(), req.GetPageSize())

	from := `
		FROM subscriptions s
		JOIN plans p ON p.id = s.plan_id
		JOIN users u ON u.id = s.user_id
		WHERE ($2 = '' OR $2 = ` + subscriptionStatusSQL + `)
		  AND ($3 = '' OR ` + ownerMatchesSQL("$3") + `)`

	res := &dentalv1.ListSubscriptionsResponse{}
	var total int64
	if err := s.d.DB.QueryRow(ctx, `SELECT count(*)`+from, now, status, query).Scan(&total); err != nil {
		return nil, internal(err)
	}
	res.TotalCount = uint32(total)

	rows, err := s.d.DB.Query(ctx, `SELECT`+subscriptionFields+`, `+subscriptionStatusSQL+`,
		u.username, u.full_name, coalesce(u.identity, ''), u.phone, u.disabled_at IS NOT NULL`+from+`
		ORDER BY s.starts_at DESC, s.id DESC
		LIMIT $4 OFFSET $5`, now, status, query, limit, offset)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	bySubscription := map[pgtype.UUID]*dentalv1.SubscriptionListItem{}
	var ids []pgtype.UUID
	for rows.Next() {
		var statusName string
		item := &dentalv1.SubscriptionListItem{User: &dentalv1.User{}, Identity: &dentalv1.PersonIdentity{}}
		sub, err := scanSubscription(rows, &statusName,
			&item.User.Username, &item.User.FullName, &item.Identity.Number, &item.Phone, &item.UserDisabled)
		if err != nil {
			return nil, internal(err)
		}
		item.Subscription, item.Status, item.User.Id = sub, subscriptionStatusFromName(statusName), sub.GetUserId()
		id, _ := toUUID(sub.GetId())
		bySubscription[id] = item
		ids = append(ids, id)
		res.Items = append(res.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	if len(ids) == 0 {
		return res, nil
	}

	// The payments that bought them; subscriptions sold in the panel have none.
	payments, err := s.d.DB.Query(ctx, `SELECT`+paymentColumns+` WHERE p.subscription_id = ANY($1)`, ids)
	if err != nil {
		return nil, internal(err)
	}
	defer payments.Close()
	for payments.Next() {
		payment, err := scanPayment(payments)
		if err != nil {
			return nil, internal(err)
		}
		id, _ := toUUID(payment.GetSubscriptionId())
		bySubscription[id].Purchase = payment
	}
	if err := payments.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}
