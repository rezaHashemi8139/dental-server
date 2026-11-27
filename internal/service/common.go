package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

// Deps is what the services need from the outside world.
type Deps struct {
	DB     *pgxpool.Pool
	Keys   *secure.KeyWrapper
	Signer *secure.Signer
	// Signs and verifies the admin panel's session tokens (JWT).
	AdminTokens *secure.TokenSigner
	SessionTTL  time.Duration
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Error reasons. The error message is always one of these stable codes, so the
// desktop app can map it to a translated text.
const (
	reasonInvalidArgument     = "invalid_argument"
	reasonWeakPassword        = "weak_password"
	reasonInvalidInstallation = "invalid_installation"
	reasonInstallationRevoked = "installation_revoked"
	reasonInvalidCredentials  = "invalid_credentials"
	reasonUserDisabled        = "user_disabled"
	reasonNotAMember          = "not_a_member"
	reasonUsernameTaken       = "username_taken"
	reasonIdentityTaken       = "identity_taken"
	reasonIdentityMismatch    = "identity_mismatch"
	reasonIdentityRequired    = "identity_required"
	reasonLicenseNumberTaken  = "license_number_taken"
	reasonUnauthenticated     = "unauthenticated"
	reasonNotAdmin            = "not_admin"
	reasonUserNotFound        = "user_not_found"
	reasonAlreadyMember       = "already_member"
	reasonCannotRemoveOwner   = "cannot_remove_owner"
	reasonOwnerAlwaysAdmin    = "owner_always_admin"
	reasonRotationMismatch    = "rotation_mismatch"
	reasonLanguageNotFound    = "language_not_found"
	reasonInternal            = "internal"
	reasonUnavailable         = "unavailable"

	reasonNoClinic               = "no_clinic"
	reasonClinicNotFound         = "clinic_not_found"
	reasonCityNotFound           = "city_not_found"
	reasonNotClinicOwner         = "not_clinic_owner"
	reasonPasswordChangeRequired = "password_change_required"
	reasonPasswordUnchanged      = "password_unchanged"
	reasonSubscriptionInactive   = "subscription_inactive"
	reasonClinicLimitExceeded    = "clinic_limit_exceeded"
	reasonDeviceLimitExceeded    = "device_limit_exceeded"
	reasonPlanNotFound           = "plan_not_found"
	reasonPlanInactive           = "plan_inactive"
	reasonSubscriptionNotFound   = "subscription_not_found"
	reasonPatientNotFound        = "patient_not_found"
	reasonUpdateRequired         = "update_required"
	reasonSubscriptionRequired   = "subscription_required"
	reasonPaymentPending         = "payment_pending"
	reasonPaymentNotFound        = "payment_not_found"
	reasonPaymentNotPending      = "payment_not_pending"
	reasonPaymentAccountNotFound = "payment_account_not_found"
	reasonRenewalQueued          = "renewal_queued"
	reasonPaymentRequired        = "payment_required"
	reasonPaymentNotRequired     = "payment_not_required"
	reasonPurchaseLimitReached   = "purchase_limit_reached"
)

// querier is what both a connection pool and a transaction offer, for helpers
// that work inside or outside a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// noDatabaseProcedures work while the database is down.
var noDatabaseProcedures = map[string]bool{
	dentalv1connect.HealthServiceCheckProcedure:           true,
	dentalv1connect.LanguageServiceListLanguagesProcedure: true,
	dentalv1connect.LanguageServiceGetLanguageProcedure:   true,
}

// ReadyInterceptor rejects calls with Unavailable while the database is not ready
// (notReady returns non-nil). HealthService.Check still answers, reporting database_ok = false,
// and so does LanguageService, which needs no database.
func ReadyInterceptor(notReady func() error) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if !noDatabaseProcedures[req.Spec().Procedure] && notReady() != nil {
				return nil, fail(connect.CodeUnavailable, reasonUnavailable)
			}
			return next(ctx, req)
		}
	}
}

func fail(code connect.Code, reason string) error {
	return connect.NewError(code, errors.New(reason))
}

// internal logs the real cause and returns a generic error to the client.
func internal(err error) error {
	slog.Error("internal error", "error", err)
	return fail(connect.CodeInternal, reasonInternal)
}

func toUUID(b []byte) (pgtype.UUID, bool) {
	if len(b) != 16 {
		return pgtype.UUID{}, false
	}
	return pgtype.UUID{Bytes: [16]byte(b), Valid: true}, true
}

func uuidBytes(u pgtype.UUID) []byte {
	return u.Bytes[:]
}
