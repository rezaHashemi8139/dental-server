package service

import (
	"context"
	"errors"
	"net/http"
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
	// The installation's clinic.
	ClinicID pgtype.UUID
	IsOwner  bool
	IsAdmin  bool
}

type sessionKey struct{}

// publicProcedures can be called without a session.
var publicProcedures = map[string]bool{
	dentalv1connect.HealthServiceCheckProcedure:           true,
	dentalv1connect.InstallationServiceRegisterProcedure:  true,
	dentalv1connect.AuthServiceSignUpProcedure:            true,
	dentalv1connect.AuthServiceLoginProcedure:             true,
	dentalv1connect.AuthServiceChangePasswordProcedure:    true,
	dentalv1connect.LanguageServiceListLanguagesProcedure: true,
	dentalv1connect.LanguageServiceGetLanguageProcedure:   true,
}

// SessionInterceptor checks the `Authorization: Bearer <token>` header on every
// non-public call. A session stays valid only while it is not expired or revoked,
// the user is not disabled, the installation is not revoked, the user is still a
// member of its clinic and the clinic owner still has a subscription in force.
// (The subscription's limits on clinics and installations are checked at login.)
//
// AdminService is not served with it: see AdminSessionInterceptor.
func SessionInterceptor(d *Deps) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if publicProcedures[req.Spec().Procedure] {
				return next(ctx, req)
			}

			token, ok := bearerToken(req.Header())
			if !ok {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}

			var s Session
			err := d.DB.QueryRow(ctx, `
				SELECT s.id, s.user_id, s.installation_id, i.clinic_id, c.owner_id = s.user_id, m.is_admin
				FROM sessions s
				JOIN users u ON u.id = s.user_id
				JOIN installations i ON i.id = s.installation_id
				JOIN clinics c ON c.id = i.clinic_id
				JOIN clinic_members m ON m.clinic_id = i.clinic_id AND m.user_id = s.user_id
				WHERE s.token_hash = $1
				  AND s.revoked_at IS NULL AND s.expires_at > $2
				  AND u.disabled_at IS NULL AND i.revoked_at IS NULL
				  AND EXISTS (
				      SELECT 1 FROM subscriptions p
				      WHERE p.user_id = c.owner_id AND p.canceled_at IS NULL
				        AND p.starts_at <= $2 AND p.ends_at > $2)`,
				secure.HashToken(token), d.Now(),
			).Scan(&s.ID, &s.UserID, &s.InstallationID, &s.ClinicID, &s.IsOwner, &s.IsAdmin)
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

// AdminSession is the logged-in administrator of an AdminService request, set by AdminSessionInterceptor.
type AdminSession struct {
	ID      pgtype.UUID
	AdminID pgtype.UUID
}

type adminSessionKey struct{}

// AdminSessionInterceptor guards AdminService, which is served with it instead of
// SessionInterceptor. Every call except AdminService.Login needs the token of an
// admin session that is not expired or revoked, of an administrator who is not disabled.
func AdminSessionInterceptor(d *Deps) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().Procedure == dentalv1connect.AdminServiceLoginProcedure {
				return next(ctx, req)
			}

			token, ok := bearerToken(req.Header())
			if !ok {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}

			var s AdminSession
			err := d.DB.QueryRow(ctx, `
				SELECT s.id, s.admin_id
				FROM admin_sessions s
				JOIN admins a ON a.id = s.admin_id
				WHERE s.token_hash = $1
				  AND s.revoked_at IS NULL AND s.expires_at > $2
				  AND a.disabled_at IS NULL`,
				secure.HashToken(token), d.Now(),
			).Scan(&s.ID, &s.AdminID)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}
			if err != nil {
				return nil, internal(err)
			}

			return next(context.WithValue(ctx, adminSessionKey{}, s), req)
		}
	}
}

// adminSessionFrom returns the session set by AdminSessionInterceptor.
func adminSessionFrom(ctx context.Context) AdminSession {
	s, ok := ctx.Value(adminSessionKey{}).(AdminSession)
	if !ok {
		panic("adminSessionFrom called on a procedure without AdminSessionInterceptor")
	}
	return s
}

// bearerToken reads the `Authorization: Bearer <token>` header.
func bearerToken(h http.Header) (string, bool) {
	token, ok := strings.CutPrefix(h.Get("Authorization"), "Bearer ")
	return token, ok && token != ""
}
