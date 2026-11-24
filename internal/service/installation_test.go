package service

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/internal/secure"
)

func TestValidNationalID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"0013542419", true},
		{"0499370899", true},
		{"0084575948", true},  // remainder < 2: the check digit is the remainder itself
		{"0013542418", false}, // wrong check digit
		{"1111111111", false}, // passes the check, but never issued
		{"0000000000", false},
		{"001354241", false},   // 9 digits
		{"00135424190", false}, // 11 digits
		{"00135a2419", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := validNationalID(tt.id); got != tt.want {
			t.Errorf("validNationalID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
	for n := range 50 {
		if id := nationalID(n); !validNationalID(id) {
			t.Fatalf("test helper made an invalid national ID %q", id)
		}
	}
}

func TestRegisterClinic(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	first := e.register(1, "doctor")
	if first.GetOwner().GetUsername() != "doctor" || len(first.GetOwner().GetId()) != 16 {
		t.Fatalf("owner = %v", first.GetOwner())
	}

	// The license number is unique, not case-sensitive. A failed registration leaves
	// nothing behind: the new owner's username and national ID stay free.
	dup := registerRequest(2, "dentist")
	dup.GetClinic().LicenseNumber = "lic-1"
	_, err := e.installation.Register(ctx, dup)
	wantCode(t, err, connect.CodeAlreadyExists, reasonLicenseNumberTaken)
	e.register(2, "dentist")

	// A new owner's username and national ID must be free.
	taken := registerRequest(3, "Doctor")
	_, err = e.installation.Register(ctx, taken)
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)
	taken = registerRequest(3, "surgeon")
	taken.GetNewOwner().Identity = nationalIdentity(nationalID(1))
	_, err = e.installation.Register(ctx, taken)
	wantCode(t, err, connect.CodeAlreadyExists, reasonIdentityTaken)

	// The national ID needs a valid check digit and is always required.
	bad := registerRequest(3, "surgeon")
	bad.GetNewOwner().Identity = nationalIdentity("0013542418")
	_, err = e.installation.Register(ctx, bad)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidNationalID)
	bad.GetNewOwner().Identity = nil
	_, err = e.installation.Register(ctx, bad)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// An existing owner: the doctor opens a second clinic.
	existing := func(machine byte, username, password, nationalID string) *dentalv1.RegisterRequest {
		req := registerRequest(machine, "unused")
		req.Owner = &dentalv1.RegisterRequest_ExistingOwner{ExistingOwner: &dentalv1.ExistingOwner{
			Username: username, Password: password, Identity: nationalIdentity(nationalID),
		}}
		return req
	}
	_, err = e.installation.Register(ctx, existing(4, "doctor", "wrong-password", nationalID(1)))
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	_, err = e.installation.Register(ctx, existing(4, "nobody", "Passw0rd-nobody", nationalID(1)))
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	_, err = e.installation.Register(ctx, existing(4, "doctor", "Passw0rd-doctor", nationalID(2)))
	wantCode(t, err, connect.CodeFailedPrecondition, reasonIdentityMismatch)

	second, err := e.installation.Register(ctx, existing(4, "DOCTOR", "Passw0rd-doctor", nationalID(1)))
	if err != nil {
		t.Fatal(err)
	}
	if second.GetOwner().GetUsername() != "doctor" || string(second.GetOwner().GetId()) != string(first.GetOwner().GetId()) {
		t.Fatalf("existing owner = %v", second.GetOwner())
	}

	// The owner is already a member and admin of both clinics.
	e.subscribe("doctor", 2, 1, 30)
	for _, inst := range []*dentalv1.RegisterResponse{first, second} {
		res, err := e.login(inst, "doctor", "Passw0rd-doctor")
		if err != nil {
			t.Fatal(err)
		}
		if !res.res.GetIsOwner() || !res.res.GetIsAdmin() {
			t.Error("clinic owner is not owner and admin of the installation")
		}
	}

	// An account made before national IDs were required has none: the first clinic
	// saves it, and it must then not belong to another account.
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO users (username, password_hash, full_name) VALUES ('hygienist', $1, 'Old account')`,
		secure.HashPassword("Passw0rd-hygienist")); err != nil {
		t.Fatal(err)
	}
	_, err = e.installation.Register(ctx, existing(5, "hygienist", "Passw0rd-hygienist", nationalID(2)))
	wantCode(t, err, connect.CodeAlreadyExists, reasonIdentityTaken)
	if _, err := e.installation.Register(ctx, existing(5, "hygienist", "Passw0rd-hygienist", nationalID(5))); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := e.deps.DB.QueryRow(ctx, `SELECT identity FROM users WHERE username = 'hygienist'`).Scan(&saved); err != nil || saved != nationalID(5) {
		t.Fatalf("saved national ID = %q, %v", saved, err)
	}

	// A disabled account cannot own a new clinic.
	if _, err := e.deps.DB.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE username = 'hygienist'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.installation.Register(ctx, existing(6, "hygienist", "Passw0rd-hygienist", nationalID(5)))
	wantCode(t, err, connect.CodePermissionDenied, reasonUserDisabled)
}
