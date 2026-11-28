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

// newAdmin creates the administrator `username` (password "Passw0rd-<username>") and logs in.
func (e *testEnv) newAdmin(username string) adminSession {
	e.t.Helper()
	if _, err := AddFirstAdmin(context.Background(), e.deps.DB, &dentalv1.CreateAdminRequest{
		Username: username, Password: "Passw0rd-" + username, FullName: username,
	}); err != nil {
		e.t.Fatal(err)
	}
	s, _, err := e.adminLogin(username, "Passw0rd-"+username)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// count returns the rows of `from` (a table and optional WHERE) with the argument args.
func (e *testEnv) count(from string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.deps.DB.QueryRow(context.Background(), `SELECT count(*) FROM `+from, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// Deleting an account takes everything it owns, and only that.
func TestAdminDeleteUser(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := e.admin(e.newAdmin("root"))

	aliceToken := e.clinicToken(1, "alice")
	alice, bob := e.sync(aliceToken), e.sync(e.clinicToken(2, "bob"))
	carol := e.signUp("carol")
	if _, err := e.members(aliceToken).AddMember(ctx, &dentalv1.AddMemberRequest{UserId: carol.GetId()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	only := testPatient(personCode(5001), "Only", now.Add(-time.Hour))
	shared := testPatient(personCode(5002), "Shared", now.Add(-time.Hour))
	if _, err := alice.Push(ctx, &dentalv1.PushRequest{
		Patients: []*dentalv1.Patient{only, shared}, Treatments: []*dentalv1.Treatment{testTreatment(1, shared, "Filling", now.Add(-time.Hour))},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{shared}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO payments (user_id, plan_id, max_clinics, max_devices_per_clinic, duration_days, mode, method, price_rials, amount_rials)
		SELECT s.user_id, s.plan_id, 1, 1, 30, 'extend', 'card_to_card', 0, 0
		FROM subscriptions s JOIN users u ON u.id = s.user_id WHERE u.username = 'alice' LIMIT 1`); err != nil {
		t.Fatal(err)
	}

	var aliceID []byte
	if err := e.deps.DB.QueryRow(ctx, `SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.DeleteUser(ctx, &dentalv1.DeleteUserRequest{Id: aliceID}); err != nil {
		t.Fatal(err)
	}

	// Registering a clinic may also give its owner a free plan, with its payment: count bob's.
	bobSubscriptions := e.count(`subscriptions s JOIN users u ON u.id = s.user_id WHERE u.username = 'bob'`)
	bobPayments := e.count(`payments p JOIN users u ON u.id = p.user_id WHERE u.username = 'bob'`)
	for from, want := range map[string]int{
		`users WHERE username = 'alice'`:           0,
		`clinics WHERE name = 'Clinic alice'`:      0,
		`subscriptions`:                            bobSubscriptions,
		`payments`:                                 bobPayments,
		`installations`:                            1,
		`treatments`:                               0,
		`clinic_members`:                           1, // bob in his own clinic; carol lost alice's
		`users WHERE username IN ('bob', 'carol')`: 2,
	} {
		if got := e.count(from); got != want {
			t.Errorf("count(%s) = %d, want %d", from, got, want)
		}
	}
	// The patient only alice's clinic had is gone; the one bob's clinic has stays.
	if got := e.count(`patients WHERE id = $1`, only.GetId()); got != 0 {
		t.Errorf("patient only in the deleted clinic: %d rows, want 0", got)
	}
	if got := e.count(`patients WHERE id = $1`, shared.GetId()); got != 1 {
		t.Errorf("shared patient: %d rows, want 1", got)
	}

	// Alice's session ended with her account.
	_, err := e.authWith(aliceToken).GetProfile(ctx, &dentalv1.GetProfileRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)

	_, err = admin.DeleteUser(ctx, &dentalv1.DeleteUserRequest{Id: aliceID})
	wantCode(t, err, connect.CodeNotFound, reasonUserNotFound)
}

// Deleting a subscription takes the payment that bought it.
func TestAdminDeleteSubscription(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	admin := e.admin(e.newAdmin("root"))
	e.signUp("alice")
	e.subscribe("alice", 1, 1, 30)

	var subID []byte
	if err := e.deps.DB.QueryRow(ctx, `
		WITH s AS (SELECT id, user_id, plan_id FROM subscriptions)
		INSERT INTO payments (user_id, plan_id, max_clinics, max_devices_per_clinic, duration_days, mode, method,
		                      price_rials, amount_rials, status, subscription_id)
		SELECT user_id, plan_id, 1, 1, 30, 'extend', 'card_to_card', 0, 0, 'approved', id FROM s
		RETURNING subscription_id`).Scan(&subID); err != nil {
		t.Fatal(err)
	}

	if _, err := admin.DeleteSubscription(ctx, &dentalv1.DeleteSubscriptionRequest{Id: subID}); err != nil {
		t.Fatal(err)
	}
	if n := e.count(`subscriptions`) + e.count(`payments`); n != 0 {
		t.Errorf("%d subscriptions and payments left, want 0", n)
	}
	_, err := admin.DeleteSubscription(ctx, &dentalv1.DeleteSubscriptionRequest{Id: subID})
	wantCode(t, err, connect.CodeNotFound, reasonSubscriptionNotFound)
}

// An administrator changes their own details and password.
func TestAdminProfile(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	rootSession := e.newAdmin("root")
	root := e.admin(rootSession)
	other, _, err := e.adminLogin("root", "Passw0rd-root")
	if err != nil {
		t.Fatal(err)
	}
	e.newAdmin("sales")

	updated, err := root.UpdateProfile(ctx, &dentalv1.AdminServiceUpdateProfileRequest{Username: "boss", FullName: "The Boss"})
	if err != nil || updated.GetAdmin().GetUsername() != "boss" || updated.GetAdmin().GetFullName() != "The Boss" {
		t.Fatalf("update = %v, %v", updated, err)
	}
	_, err = root.UpdateProfile(ctx, &dentalv1.AdminServiceUpdateProfileRequest{Username: "SALES", FullName: "x"})
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)
	_, err = root.UpdateProfile(ctx, &dentalv1.AdminServiceUpdateProfileRequest{Username: "boss", FullName: ""})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// A wrong current password does not end the session.
	_, err = root.ChangePassword(ctx, &dentalv1.AdminServiceChangePasswordRequest{CurrentPassword: "nope", NewPassword: "NewPassw0rd"})
	wantCode(t, err, connect.CodePermissionDenied, reasonInvalidCredentials)
	_, err = root.ChangePassword(ctx, &dentalv1.AdminServiceChangePasswordRequest{CurrentPassword: "Passw0rd-root", NewPassword: "weak"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonWeakPassword)
	_, err = root.ChangePassword(ctx, &dentalv1.AdminServiceChangePasswordRequest{CurrentPassword: "Passw0rd-root", NewPassword: "Passw0rd-root"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonPasswordUnchanged)

	if _, err := root.ChangePassword(ctx, &dentalv1.AdminServiceChangePasswordRequest{
		CurrentPassword: "Passw0rd-root", NewPassword: "NewPassw0rd",
	}); err != nil {
		t.Fatal(err)
	}
	// This session goes on; the other one has ended.
	if _, err := root.ListPlans(ctx, &dentalv1.ListPlansRequest{}); err != nil {
		t.Fatalf("own session after password change: %v", err)
	}
	_, err = e.admin(other).ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)

	if _, _, err := e.adminLogin("boss", "Passw0rd-root"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("login with the old password: %v", err)
	}
	if _, _, err := e.adminLogin("Boss", "NewPassw0rd"); err != nil {
		t.Errorf("login with the new details: %v", err)
	}
}
