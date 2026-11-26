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
	dentalv1connect.HealthServiceCheckProcedure:               true,
	dentalv1connect.InstallationServiceRegisterProcedure:      true,
	dentalv1connect.InstallationServiceListMyClinicsProcedure: true,
	dentalv1connect.InstallationServiceListCitiesProcedure: true,
	dentalv1connect.AuthServiceSignUpProcedure:                true,
	dentalv1connect.AuthServiceLoginProcedure:                 true,
	dentalv1connect.AuthServiceChangePasswordProcedure:        true,
	dentalv1connect.LanguageServiceListLanguagesProcedure:     true,
	dentalv1connect.LanguageServiceGetLanguageProcedure:       true,
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

// How an admin session is proven; see AdminService in proto/dental/v1/admin.proto.
const (
	// adminTokenCookie holds the session's JWT.
	adminTokenCookie = "dental_admin_token"
	// csrfTokenHeader carries the session's CSRF token.
	csrfTokenHeader = "X-CSRF-Token"
	// adminTokenAudience keeps admin tokens from being accepted for anything else.
	adminTokenAudience = "dental-admin"
)

// adminCookie returns the cookie that holds an admin session's JWT for maxAge
// seconds. A negative maxAge removes it.
func adminCookie(token string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:  adminTokenCookie,
		Value: token,
		// Sent only to AdminService, never to the other services.
		Path:     "/" + dentalv1connect.AdminServiceName + "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	}
}

// setCookie adds a cookie to the response of the current handler call.
func setCookie(ctx context.Context, cookie *http.Cookie) error {
	call, ok := connect.CallInfoForHandlerContext(ctx)
	if !ok {
		return errors.New("no handler call info: cannot set a cookie")
	}
	call.ResponseHeader().Add("Set-Cookie", cookie.String())
	return nil
}

// cookieValue reads a cookie from the request headers.
func cookieValue(h http.Header, name string) (string, bool) {
	cookie, err := (&http.Request{Header: h}).Cookie(name)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

// AdminSessionInterceptor guards AdminService, which is served with it instead of
// SessionInterceptor. Every call except AdminService.Login needs the session's JWT
// (cookie `dental_admin_token`, signed by this server, not expired) and its CSRF
// token (header `X-CSRF-Token`), of an admin session that is not expired or revoked,
// of an administrator who is not disabled. The session is looked up on every call,
// so a token stops working as soon as its session ends.
func AdminSessionInterceptor(d *Deps) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().Procedure == dentalv1connect.AdminServiceLoginProcedure {
				return next(ctx, req)
			}

			token, ok := cookieValue(req.Header(), adminTokenCookie)
			csrfToken := req.Header().Get(csrfTokenHeader)
			if !ok || csrfToken == "" {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}
			now := d.Now()
			claims, err := d.AdminTokens.Verify(token, adminTokenAudience, now)
			if err != nil {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}
			var sessionID, adminID pgtype.UUID
			if sessionID.Scan(claims.SessionID) != nil || adminID.Scan(claims.Subject) != nil {
				return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
			}

			var s AdminSession
			err = d.DB.QueryRow(ctx, `
				SELECT s.id, s.admin_id
				FROM admin_sessions s
				JOIN admins a ON a.id = s.admin_id
				WHERE s.id = $1 AND s.admin_id = $2 AND s.csrf_hash = $3
				  AND s.revoked_at IS NULL AND s.expires_at > $4
				  AND a.disabled_at IS NULL`,
				sessionID, adminID, secure.HashToken(csrfToken), now,
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
