package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

// Deps is what the services need from the outside world.
type Deps struct {
	DB         *pgxpool.Pool
	Keys       *secure.KeyWrapper
	Signer     *secure.Signer
	SessionTTL time.Duration
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
	reasonUnauthenticated     = "unauthenticated"
	reasonNotAdmin            = "not_admin"
	reasonUserNotFound        = "user_not_found"
	reasonAlreadyMember       = "already_member"
	reasonCannotRemoveOwner   = "cannot_remove_owner"
	reasonOwnerAlwaysAdmin    = "owner_always_admin"
	reasonRotationMismatch    = "rotation_mismatch"
	reasonInternal            = "internal"
	reasonUnavailable         = "unavailable"
)

// ReadyInterceptor rejects calls with Unavailable while the database is not ready
// (notReady returns non-nil). HealthService.Check still answers, reporting database_ok = false.
func ReadyInterceptor(notReady func() error) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().Procedure != dentalv1connect.HealthServiceCheckProcedure && notReady() != nil {
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
