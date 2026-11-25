package service

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/joho/godotenv"
	"google.golang.org/protobuf/proto"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/database"
	"dental-server/internal/secure"
)

// testEnv is a server on an empty test database, reached through real gRPC clients.
type testEnv struct {
	t            *testing.T
	deps         *Deps
	server       *httptest.Server
	installation dentalv1connect.InstallationServiceClient
	auth         dentalv1connect.AuthServiceClient
	signerPublic ed25519.PublicKey
	// Distinct national IDs for signUp, above the range used by registerRequest.
	nextPerson int
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	_ = godotenv.Load("../../.env")
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	db, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Exec(ctx, `TRUNCATE users, clinics, installations, clinic_members, installation_keys, sessions,
		admins, admin_sessions, plans, subscriptions, patients, clinic_patients, treatments, patient_history_views
		CASCADE`); err != nil {
		t.Fatal(err)
	}

	keys, _ := secure.NewKeyWrapper(secure.RandomBytes(32))
	signer, _ := secure.NewSigner(secure.RandomBytes(32))
	adminTokens, _ := secure.NewTokenSigner(secure.RandomBytes(32))
	deps := &Deps{DB: db, Keys: keys, Signer: signer, AdminTokens: adminTokens, SessionTTL: time.Hour, Now: time.Now}

	interceptors := connect.WithInterceptors(ValidateInterceptor(), SessionInterceptor(deps))
	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewInstallationServiceHandler(NewInstallationService(deps), interceptors))
	mux.Handle(dentalv1connect.NewAuthServiceHandler(NewAuthService(deps), interceptors))
	mux.Handle(dentalv1connect.NewMemberServiceHandler(NewMemberService(deps), interceptors))
	mux.Handle(dentalv1connect.NewSyncServiceHandler(NewSyncService(deps), interceptors))
	mux.Handle(dentalv1connect.NewPatientServiceHandler(NewPatientService(deps), interceptors))
	mux.Handle(dentalv1connect.NewAdminServiceHandler(NewAdminService(deps),
		connect.WithInterceptors(ValidateInterceptor(), AdminSessionInterceptor(deps))))

	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	return &testEnv{
		t: t, deps: deps, server: server,
		installation: dentalv1connect.NewInstallationServiceClient(server.Client(), server.URL, connect.WithGRPC()),
		auth:         dentalv1connect.NewAuthServiceClient(server.Client(), server.URL, connect.WithGRPC()),
		signerPublic: signer.PublicKey(),
		nextPerson:   1_000_000,
	}
}

// adminSession is what an AdminService client sends: the JWT that Login set as
// a cookie and the CSRF token it returned. Empty values are not sent.
type adminSession struct {
	token, csrf string
}

// admin returns an AdminService client that sends the given admin session.
func (e *testEnv) admin(s adminSession) dentalv1connect.AdminServiceClient {
	return dentalv1connect.NewAdminServiceClient(e.server.Client(), e.server.URL, connect.WithGRPC(),
		connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if s.token != "" {
					req.Header().Add("Cookie", (&http.Cookie{Name: adminTokenCookie, Value: s.token}).String())
				}
				if s.csrf != "" {
					req.Header().Set(csrfTokenHeader, s.csrf)
				}
				return next(ctx, req)
			}
		})))
}

// adminLogin logs an administrator in. It returns the session as a browser would
// keep it and the session cookie exactly as the server set it.
func (e *testEnv) adminLogin(username, password string) (adminSession, *http.Cookie, error) {
	e.t.Helper()
	ctx, call := connect.NewClientContext(context.Background())
	res, err := e.admin(adminSession{}).Login(ctx, &dentalv1.AdminServiceLoginRequest{Username: username, Password: password})
	if err != nil {
		return adminSession{}, nil, err
	}
	cookie := responseCookie(e.t, call.ResponseHeader(), adminTokenCookie)
	return adminSession{token: cookie.Value, csrf: res.GetCsrfToken()}, cookie, nil
}

// responseCookie returns the cookie `name` set by a response; the test fails without one.
func responseCookie(t *testing.T, h http.Header, name string) *http.Cookie {
	t.Helper()
	for _, line := range h.Values("Set-Cookie") {
		if cookie, err := http.ParseSetCookie(line); err == nil && cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("no %s cookie in %q", name, h.Values("Set-Cookie"))
	return nil
}

// subscribe gives the user `username` a subscription in force from an hour ago
// for `days` days, as if sold from the admin panel.
func (e *testEnv) subscribe(username string, maxClinics, maxDevices, days int) {
	e.t.Helper()
	ctx := context.Background()
	var planID [16]byte
	if err := e.deps.DB.QueryRow(ctx, `
		INSERT INTO plans (name, max_clinics, max_devices_per_clinic, duration_days)
		VALUES ('test', $1, $2, $3) RETURNING id`, maxClinics, maxDevices, days).Scan(&planID); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO subscriptions (user_id, plan_id, max_clinics, max_devices_per_clinic, price_rials, starts_at, ends_at)
		SELECT u.id, $2, $3, $4, 0, now() - interval '1 hour', now() - interval '1 hour' + make_interval(days => $5)
		FROM users u WHERE lower(u.username) = lower($1)`, username, planID, maxClinics, maxDevices, days); err != nil {
		e.t.Fatal(err)
	}
}

// members returns a MemberService client that sends the given session token.
func (e *testEnv) members(token string) dentalv1connect.MemberServiceClient {
	return dentalv1connect.NewMemberServiceClient(e.server.Client(), e.server.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
}

func (e *testEnv) authWith(token string) dentalv1connect.AuthServiceClient {
	return dentalv1connect.NewAuthServiceClient(e.server.Client(), e.server.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
}

func bearer(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}

// registerRequest registers a clinic with license number "LIC-<machine>" whose owner is
// a new account `owner` (password "Passw0rd-<owner>", national ID nationalID(machine)).
func registerRequest(machine byte, owner string) *dentalv1.RegisterRequest {
	return &dentalv1.RegisterRequest{
		Machine: &dentalv1.MachineInfo{
			MachineIdHash: bytes.Repeat([]byte{machine}, 32),
			Hostname:      "clinic-pc",
			MacAddresses:  []string{"AA:BB:CC:DD:EE:FF"},
		},
		Target: &dentalv1.RegisterRequest_Clinic{Clinic: &dentalv1.ClinicInfo{
			Name:          "Clinic " + owner,
			LicenseNumber: fmt.Sprintf("LIC-%d", machine),
			Phone:         "02112345678",
			City:          "Tehran",
			Address:       "1 Test Street",
		}},
		Owner: &dentalv1.RegisterRequest_NewOwner{NewOwner: &dentalv1.NewOwner{
			Username: owner,
			Password: "Passw0rd-" + owner,
			FullName: "Dr. " + owner,
			Identity: nationalIdentity(nationalID(int(machine))),
			Phone:    "09121234567",
		}},
	}
}

// register registers an installation on `machine` whose new owner is `owner` (see registerRequest).
func (e *testEnv) register(machine byte, owner string) *dentalv1.RegisterResponse {
	e.t.Helper()
	res, err := e.installation.Register(context.Background(), registerRequest(machine, owner))
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// nationalID returns a valid national ID that differs for every n below 10^8.
func nationalID(n int) string {
	digits := fmt.Sprintf("1%08d", n)
	sum := 0
	for i := range 9 {
		sum += int(digits[i]-'0') * (10 - i)
	}
	check := sum % 11
	if check >= 2 {
		check = 11 - check
	}
	return fmt.Sprintf("%s%d", digits, check)
}

// signUp creates the account `username` (password "Passw0rd-<username>") with a fresh national ID.
func (e *testEnv) signUp(username string) *dentalv1.User {
	e.t.Helper()
	e.nextPerson++
	res, err := e.auth.SignUp(context.Background(), &dentalv1.SignUpRequest{
		Username: username, Password: "Passw0rd-" + username, FullName: "Dr. " + username,
		Identity: nationalIdentity(nationalID(e.nextPerson)), Phone: "09121234567",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.GetUser()
}

type loginResult struct {
	res   *dentalv1.LoginResponse
	keys  *dentalv1.DatabaseKeys
	grant *dentalv1.OfflineGrant
}

// login logs in like the desktop app: with a one-time key, then opens the keys and checks the grant.
func (e *testEnv) login(inst *dentalv1.RegisterResponse, username, password string) (*loginResult, error) {
	e.t.Helper()
	clientKey, _ := ecdh.X25519().GenerateKey(rand.Reader)
	res, err := e.auth.Login(context.Background(), &dentalv1.LoginRequest{
		InstallationId: inst.GetInstallationId(), InstallationSecret: inst.GetInstallationSecret(),
		Username: username, Password: password, ClientPublicKey: clientKey.PublicKey().Bytes(),
	})
	if err != nil {
		return nil, err
	}

	plain, err := secure.OpenSealed(clientKey, res.GetDatabaseKeys(), inst.GetInstallationId())
	if err != nil {
		e.t.Fatalf("open sealed keys: %v", err)
	}
	keys := &dentalv1.DatabaseKeys{}
	if err := proto.Unmarshal(plain, keys); err != nil {
		e.t.Fatal(err)
	}

	signed := res.GetOfflineGrant()
	if !ed25519.Verify(e.signerPublic, signed.GetGrant(), signed.GetSignature()) {
		e.t.Fatal("offline grant signature invalid")
	}
	grant := &dentalv1.OfflineGrant{}
	if err := proto.Unmarshal(signed.GetGrant(), grant); err != nil {
		e.t.Fatal(err)
	}
	return &loginResult{res: res, keys: keys, grant: grant}, nil
}

func wantCode(t *testing.T, err error, code connect.Code, reason string) {
	t.Helper()
	connectErr, ok := errors.AsType[*connect.Error](err)
	if !ok || connectErr.Code() != code || connectErr.Message() != reason {
		t.Fatalf("error = %v, want %v %q", err, code, reason)
	}
}

func TestLoginKeyRotationAndMembers(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	inst := e.register(1, "doctor")
	if inst.GetKnownMachine() {
		t.Error("first installation reported as known machine")
	}
	again := registerRequest(1, "dentist")
	again.GetClinic().LicenseNumber = "LIC-1-second"
	again.GetNewOwner().Identity = nationalIdentity(nationalID(1001))
	if res, err := e.installation.Register(ctx, again); err != nil || !res.GetKnownMachine() {
		t.Errorf("second installation on the same machine not recognised (%v)", err)
	}

	signUp := func(username, id string) error {
		_, err := e.auth.SignUp(ctx, &dentalv1.SignUpRequest{
			Username: username, Password: "Another-passw0rd", FullName: "Someone", Identity: nationalIdentity(id), Phone: "09121234567",
		})
		return err
	}
	wantCode(t, signUp("DOCTOR", nationalID(2001)), connect.CodeAlreadyExists, reasonUsernameTaken)
	wantCode(t, signUp("someone", nationalID(1)), connect.CodeAlreadyExists, reasonIdentityTaken)
	wantCode(t, signUp("someone", "0013542418"), connect.CodeInvalidArgument, reasonInvalidNationalID)
	_, err := e.auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "x", Password: "short"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// Nobody can log in until the clinic owner has a subscription.
	_, err = e.login(inst, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodePermissionDenied, reasonSubscriptionInactive)
	e.subscribe("doctor", 1, 2, 30)

	// First login: the doctor is already owner, the database does not exist yet.
	first, err := e.login(inst, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !first.res.GetIsOwner() || !first.res.GetIsAdmin() {
		t.Error("first user is not owner and admin")
	}
	if first.keys.GetCurrentVersion() != 0 || len(first.keys.GetCurrentKey()) != 0 || len(first.keys.GetNextKey()) != 32 {
		t.Fatalf("first keys = %v", first.keys)
	}
	if !first.grant.GetExpiresAt().AsTime().After(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("grant expires %v, want about 7 days from now", first.grant.GetExpiresAt().AsTime())
	}

	// A retried login before confirming gets the same pending key.
	retry, err := e.login(inst, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(retry.keys.GetNextKey(), first.keys.GetNextKey()) {
		t.Error("pending key changed before confirmation")
	}

	// Confirm: next becomes current (version 1). Confirming again is harmless.
	doctor := e.authWith(first.res.GetSessionToken())
	if res, err := doctor.ConfirmKeyRotation(ctx, &dentalv1.ConfirmKeyRotationRequest{NewVersion: 1}); err != nil || res.GetCurrentVersion() != 1 {
		t.Fatalf("confirm = %v, %v", res, err)
	}
	if _, err := doctor.ConfirmKeyRotation(ctx, &dentalv1.ConfirmKeyRotationRequest{NewVersion: 1}); err != nil {
		t.Fatalf("repeated confirm: %v", err)
	}
	_, err = doctor.ConfirmKeyRotation(ctx, &dentalv1.ConfirmKeyRotationRequest{NewVersion: 5})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonRotationMismatch)

	// Next login: current is the confirmed key, and there is a new next key.
	second, err := e.login(inst, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	if second.keys.GetCurrentVersion() != 1 || !bytes.Equal(second.keys.GetCurrentKey(), first.keys.GetNextKey()) {
		t.Fatalf("second login current key = v%d", second.keys.GetCurrentVersion())
	}
	if bytes.Equal(second.keys.GetNextKey(), first.keys.GetNextKey()) {
		t.Error("no new key after rotation")
	}

	// Wrong password, wrong installation secret, unknown user.
	_, err = e.login(inst, "doctor", "wrong-password")
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	_, err = e.login(inst, "nobody", "Passw0rd-nobody")
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	forged := proto.Clone(inst).(*dentalv1.RegisterResponse)
	forged.InstallationSecret = bytes.Repeat([]byte{9}, 32)
	_, err = e.login(forged, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidInstallation)

	// A user who is not a member cannot get the keys.
	secretaryUser := e.signUp("secretary")
	_, err = e.login(inst, "secretary", "Passw0rd-secretary")
	wantCode(t, err, connect.CodePermissionDenied, reasonNotAMember)

	// The admin finds them by national ID and adds them; then they get the same database key.
	owner := e.members(second.res.GetSessionToken())
	found, err := owner.FindUser(ctx, &dentalv1.FindUserRequest{Identity: nationalIdentity(nationalID(e.nextPerson))})
	if err != nil || !bytes.Equal(found.GetUser().GetId(), secretaryUser.GetId()) {
		t.Fatalf("find user = %v, %v", found, err)
	}
	_, err = owner.FindUser(ctx, &dentalv1.FindUserRequest{Identity: nationalIdentity(nationalID(9999))})
	wantCode(t, err, connect.CodeNotFound, reasonUserNotFound)
	added, err := owner.AddMember(ctx, &dentalv1.AddMemberRequest{UserId: found.GetUser().GetId()})
	if err != nil || added.GetMember().GetIsAdmin() || added.GetMember().GetUser().GetUsername() != "secretary" {
		t.Fatalf("add member = %v, %v", added, err)
	}
	_, err = owner.AddMember(ctx, &dentalv1.AddMemberRequest{UserId: found.GetUser().GetId()})
	wantCode(t, err, connect.CodeAlreadyExists, reasonAlreadyMember)
	_, err = owner.AddMember(ctx, &dentalv1.AddMemberRequest{UserId: bytes.Repeat([]byte{7}, 16)})
	wantCode(t, err, connect.CodeNotFound, reasonUserNotFound)

	sec, err := e.login(inst, "secretary", "Passw0rd-secretary")
	if err != nil {
		t.Fatal(err)
	}
	if sec.res.GetIsOwner() || sec.res.GetIsAdmin() || !bytes.Equal(sec.keys.GetCurrentKey(), second.keys.GetCurrentKey()) {
		t.Errorf("secretary login: owner=%v admin=%v same key=%v", sec.res.GetIsOwner(), sec.res.GetIsAdmin(),
			bytes.Equal(sec.keys.GetCurrentKey(), second.keys.GetCurrentKey()))
	}

	// A non-admin cannot manage members; nobody can remove the owner.
	secretary := e.members(sec.res.GetSessionToken())
	_, err = secretary.AddMember(ctx, &dentalv1.AddMemberRequest{UserId: second.res.GetUser().GetId()})
	wantCode(t, err, connect.CodePermissionDenied, reasonNotAdmin)
	_, err = secretary.FindUser(ctx, &dentalv1.FindUserRequest{Identity: nationalIdentity(nationalID(1))})
	wantCode(t, err, connect.CodePermissionDenied, reasonNotAdmin)
	list, err := secretary.ListMembers(ctx, &dentalv1.ListMembersRequest{})
	if err != nil || len(list.GetMembers()) != 2 {
		t.Fatalf("list members = %v, %v", list, err)
	}
	_, err = owner.RemoveMember(ctx, &dentalv1.RemoveMemberRequest{UserId: second.res.GetUser().GetId()})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonCannotRemoveOwner)
	_, err = owner.SetMemberAdmin(ctx, &dentalv1.SetMemberAdminRequest{UserId: second.res.GetUser().GetId(), IsAdmin: false})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonOwnerAlwaysAdmin)

	// Removing the secretary ends their session at once.
	if _, err := owner.RemoveMember(ctx, &dentalv1.RemoveMemberRequest{UserId: sec.res.GetUser().GetId()}); err != nil {
		t.Fatal(err)
	}
	_, err = secretary.ListMembers(ctx, &dentalv1.ListMembersRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)

	// Logout ends the session; a revoked installation gets nothing.
	if _, err := doctor.Logout(ctx, &dentalv1.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	_, err = doctor.ConfirmKeyRotation(ctx, &dentalv1.ConfirmKeyRotationRequest{NewVersion: 1})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)

	installationID, _ := toUUID(inst.GetInstallationId())
	if _, err := e.deps.DB.Exec(ctx, `UPDATE installations SET revoked_at = now() WHERE id = $1`, installationID); err != nil {
		t.Fatal(err)
	}
	_, err = e.login(inst, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodePermissionDenied, reasonInstallationRevoked)
	_, err = owner.ListMembers(ctx, &dentalv1.ListMembersRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}

func TestKeysAreEncryptedAtRest(t *testing.T) {
	e := newTestEnv(t)
	inst := e.register(2, "doctor")
	e.subscribe("doctor", 1, 1, 30)
	res, err := e.login(inst, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}

	var stored []byte
	installationID, _ := toUUID(inst.GetInstallationId())
	if err := e.deps.DB.QueryRow(context.Background(),
		`SELECT next_key FROM installation_keys WHERE installation_id = $1`, installationID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, res.keys.GetNextKey()) {
		t.Error("database key stored in plain form")
	}
}

// nationalIdentity is the identity of an Iranian citizen with national ID id.
func nationalIdentity(id string) *dentalv1.PersonIdentity {
	return &dentalv1.PersonIdentity{Type: dentalv1.IdentityType_IDENTITY_TYPE_NATIONAL, Number: id, Nationality: "IR"}
}
