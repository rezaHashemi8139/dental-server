package service

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
	if _, err := db.Exec(ctx, `TRUNCATE users, installations, installation_members, installation_keys, sessions CASCADE`); err != nil {
		t.Fatal(err)
	}

	keys, _ := secure.NewKeyWrapper(secure.RandomBytes(32))
	signer, _ := secure.NewSigner(secure.RandomBytes(32))
	deps := &Deps{DB: db, Keys: keys, Signer: signer, SessionTTL: time.Hour, Now: time.Now}

	interceptors := connect.WithInterceptors(SessionInterceptor(deps))
	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewInstallationServiceHandler(NewInstallationService(deps), interceptors))
	mux.Handle(dentalv1connect.NewAuthServiceHandler(NewAuthService(deps), interceptors))
	mux.Handle(dentalv1connect.NewMemberServiceHandler(NewMemberService(deps), interceptors))

	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	return &testEnv{
		t: t, deps: deps, server: server,
		installation: dentalv1connect.NewInstallationServiceClient(server.Client(), server.URL, connect.WithGRPC()),
		auth:         dentalv1connect.NewAuthServiceClient(server.Client(), server.URL, connect.WithGRPC()),
		signerPublic: signer.PublicKey(),
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

func (e *testEnv) register(machine byte) *dentalv1.RegisterResponse {
	e.t.Helper()
	res, err := e.installation.Register(context.Background(), &dentalv1.RegisterRequest{Machine: &dentalv1.MachineInfo{
		MachineIdHash: bytes.Repeat([]byte{machine}, 32),
		Hostname:      "clinic-pc",
		MacAddresses:  []string{"AA:BB:CC:DD:EE:FF"},
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *testEnv) signUp(username string) {
	e.t.Helper()
	if _, err := e.auth.SignUp(context.Background(), &dentalv1.SignUpRequest{
		Username: username, Password: "Passw0rd-" + username, FullName: "Dr. " + username,
	}); err != nil {
		e.t.Fatal(err)
	}
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

	inst := e.register(1)
	if inst.GetKnownMachine() {
		t.Error("first installation reported as known machine")
	}
	if e.register(1).GetKnownMachine() != true {
		t.Error("second installation on the same machine not recognised")
	}

	e.signUp("doctor")
	_, err := e.auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "DOCTOR", Password: "Another-passw0rd"})
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)
	_, err = e.auth.SignUp(ctx, &dentalv1.SignUpRequest{Username: "x", Password: "short"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// First login: the doctor becomes owner, the database does not exist yet.
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
	e.signUp("secretary")
	_, err = e.login(inst, "secretary", "Passw0rd-secretary")
	wantCode(t, err, connect.CodePermissionDenied, reasonNotAMember)

	// The admin adds them; then they get the same database key.
	owner := e.members(second.res.GetSessionToken())
	added, err := owner.AddMember(ctx, &dentalv1.AddMemberRequest{Username: "Secretary"})
	if err != nil || added.GetMember().GetIsAdmin() {
		t.Fatalf("add member = %v, %v", added, err)
	}
	_, err = owner.AddMember(ctx, &dentalv1.AddMemberRequest{Username: "secretary"})
	wantCode(t, err, connect.CodeAlreadyExists, reasonAlreadyMember)
	_, err = owner.AddMember(ctx, &dentalv1.AddMemberRequest{Username: "ghost"})
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
	_, err = secretary.AddMember(ctx, &dentalv1.AddMemberRequest{Username: "doctor"})
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
	inst := e.register(2)
	e.signUp("doctor")
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
