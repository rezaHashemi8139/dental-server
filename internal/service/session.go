package service

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

// Session is the logged-in user of a request, set by SessionInterceptor.
type Session struct {
	ID             pgtype.UUID
	UserID         pgtype.UUID
	InstallationID pgtype.UUID
	IsOwner        bool
	IsAdmin        bool
}

type sessionKey struct{}

// publicProcedures can be called without a session.
var publicProcedures = map[string]bool{
	dentalv1connect.HealthServiceCheckProcedure:          true,
	dentalv1connect.InstallationServiceRegisterProcedure: true,
	dentalv1connect.AuthServiceSignUpProcedure:           true,
	dentalv1connect.AuthServiceLoginProcedure:            true,
}

// SessionInterceptor checks the `Authorization: Bearer <token>` header on every
// non-public call. A session stays valid only while it is not expired or revoked,
// the user is not disabled, the installation is not revoked and the user is still
// a member of it.
func SessionInterceptor(d *Deps) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if publicProcedures[req.Spec().Procedure] {
				return next(ctx, req)
			}

			token, ok := strings.CutPrefix(req.Header().Get("Authorization"), "Bearer ")
			if !ok || token == "" {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}

			var s Session
			err := d.DB.QueryRow(ctx, `
				SELECT s.id, s.user_id, s.installation_id, i.owner_id = s.user_id, m.is_admin
				FROM sessions s
				JOIN users u ON u.id = s.user_id
				JOIN installations i ON i.id = s.installation_id
				JOIN installation_members m ON m.installation_id = s.installation_id AND m.user_id = s.user_id
				WHERE s.token_hash = $1
				  AND s.revoked_at IS NULL AND s.expires_at > $2
				  AND u.disabled_at IS NULL AND i.revoked_at IS NULL`,
				secure.HashToken(token), d.Now(),
			).Scan(&s.ID, &s.UserID, &s.InstallationID, &s.IsOwner, &s.IsAdmin)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}
			if err != nil {
				return nil, internal(err)
			}

			return next(context.WithValue(ctx, sessionKey{}, s), req)
		}
	}
}

// sessionFrom returns the session set by SessionInterceptor.
func sessionFrom(ctx context.Context) Session {
	s, ok := ctx.Value(sessionKey{}).(Session)
	if !ok {
		panic("sessionFrom called on a procedure without SessionInterceptor")
	}
	return s
}
