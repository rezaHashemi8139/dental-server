package service

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
)

func TestProfile(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	inst := e.register(1, "owner")
	e.subscribe("owner", 1, 1, 30)
	login, err := e.login(inst, "owner", "Passw0rd-owner")
	if err != nil {
		t.Fatal(err)
	}
	auth := e.authWith(login.res.GetSessionToken())

	got, err := auth.GetProfile(ctx, &dentalv1.GetProfileRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p := got.GetProfile()
	if p.GetUser().GetUsername() != "owner" || p.GetUser().GetFullName() != "Dr. owner" ||
		p.GetIdentity().GetNumber() != personCode(1) || p.GetPhone() != "09121234567" {
		t.Fatalf("profile = %v", p)
	}

	updated, err := auth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{FullName: "Dr. New Name", Phone: "+989121112233"})
	if err != nil {
		t.Fatal(err)
	}
	p = updated.GetProfile()
	if p.GetUser().GetFullName() != "Dr. New Name" || p.GetPhone() != "+989121112233" || p.GetUser().GetUsername() != "owner" {
		t.Fatalf("updated profile = %v", p)
	}

	// The next login carries the new name, in the offline grant too.
	again, err := e.login(inst, "owner", "Passw0rd-owner")
	if err != nil {
		t.Fatal(err)
	}
	if again.res.GetUser().GetFullName() != "Dr. New Name" || again.grant.GetFullName() != "Dr. New Name" {
		t.Fatalf("login after update: user %v, grant %v", again.res.GetUser(), again.grant)
	}

	// Checked like sign-up.
	_, err = auth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{FullName: "", Phone: "09121234567"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty name: error = %v, want invalid argument", err)
	}
	_, err = auth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{FullName: "Dr. A", Phone: "12-34"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad phone: error = %v, want invalid argument", err)
	}

	// Needs a session.
	_, err = e.auth.GetProfile(ctx, &dentalv1.GetProfileRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}
