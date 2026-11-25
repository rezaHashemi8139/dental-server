package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

// An admin session needs both its JWT cookie and its CSRF token, and ends at once
// on logout or when the administrator is disabled, although the JWT has not expired.
func TestAdminSessionTokens(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	for _, username := range []string{"root", "sales"} {
		if _, err := AddFirstAdmin(ctx, e.deps.DB, &dentalv1.CreateAdminRequest{
			Username: username, Password: "Passw0rd-" + username, FullName: username,
		}); err != nil {
			t.Fatal(err)
		}
	}

	root, cookie, err := e.adminLogin("root", "Passw0rd-root")
	if err != nil {
		t.Fatal(err)
	}
	// Out of reach of scripts and of other sites, and only sent to AdminService.
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode ||
		cookie.Path != "/"+dentalv1connect.AdminServiceName+"/" || cookie.MaxAge != int(time.Hour/time.Second) {
		t.Errorf("session cookie = %q", cookie.String())
	}
	if root.csrf == "" || strings.Count(root.token, ".") != 2 {
		t.Fatalf("session = %+v, want a JWT and a CSRF token", root)
	}
	if _, err := e.admin(root).ListPlans(ctx, &dentalv1.ListPlansRequest{}); err != nil {
		t.Fatal(err)
	}

	sales, _, err := e.adminLogin("sales", "Passw0rd-sales")
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _ := secure.NewTokenSigner(secure.RandomBytes(32))
	forged, _ := otherKey.Sign(secure.TokenClaims{Audience: adminTokenAudience, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	for name, s := range map[string]adminSession{
		"no CSRF token":               {token: root.token},
		"no cookie":                   {csrf: root.csrf},
		"wrong CSRF token":            {token: root.token, csrf: root.csrf + "x"},
		"another session's CSRF":      {token: root.token, csrf: sales.csrf},
		"changed token":               {token: root.token + "x", csrf: root.csrf},
		"token signed by another key": {token: forged, csrf: root.csrf},
	} {
		_, err := e.admin(s).ListPlans(ctx, &dentalv1.ListPlansRequest{})
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s: err = %v, want unauthenticated", name, err)
		}
	}

	// Past its expiry the token is refused, whatever the database says.
	e.deps.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, err = e.admin(root).ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
	e.deps.Now = time.Now

	// Logout removes the cookie and ends the session on the server: the same
	// token and CSRF token no longer work. Other sessions are not touched.
	logoutCtx, call := connect.NewClientContext(ctx)
	if _, err := e.admin(root).Logout(logoutCtx, &dentalv1.AdminServiceLogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	if cleared := responseCookie(t, call.ResponseHeader(), adminTokenCookie); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("logout cookie = %q, want it removed", cleared.String())
	}
	_, err = e.admin(root).ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
	if _, err := e.admin(sales).ListPlans(ctx, &dentalv1.ListPlansRequest{}); err != nil {
		t.Fatalf("other session after logout: %v", err)
	}

	// A disabled administrator's sessions end at once.
	if _, err := e.deps.DB.Exec(ctx, `UPDATE admins SET disabled_at = now() WHERE username = 'sales'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.admin(sales).ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}
