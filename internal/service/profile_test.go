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
		p.GetIdentity().GetNumber() != personCode(1) || p.GetPhone() != "09121234567" || p.GetEmail() != "" {
		t.Fatalf("profile = %v", p)
	}

	updated, err := auth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{
		FullName: "Dr. New Name", Phone: "+989121112233", Email: "owner@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	p = updated.GetProfile()
	if p.GetUser().GetFullName() != "Dr. New Name" || p.GetPhone() != "+989121112233" ||
		p.GetEmail() != "owner@example.com" || p.GetUser().GetUsername() != "owner" {
		t.Fatalf("updated profile = %v", p)
	}
	got, err = auth.GetProfile(ctx, &dentalv1.GetProfileRequest{})
	if err != nil || got.GetProfile().GetEmail() != "owner@example.com" {
		t.Fatalf("profile after update = %v, %v", got.GetProfile(), err)
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
	_, err = auth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{FullName: "Dr. A", Phone: "09121234567", Email: "owner@"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad email: error = %v, want invalid argument", err)
	}

	// The email is optional: an empty one removes it.
	cleared, err := auth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{FullName: "Dr. New Name", Phone: "09121234567"})
	if err != nil || cleared.GetProfile().GetEmail() != "" {
		t.Fatalf("clear email: profile = %v, error = %v", cleared.GetProfile(), err)
	}

	// Set at sign-up, and shared by another account: emails are not unique.
	for _, username := range []string{"first", "second"} {
		e.nextPerson++
		_, err = e.auth.SignUp(ctx, &dentalv1.SignUpRequest{
			Username: username, Password: "Passw0rd-" + username, FullName: "Dr. " + username,
			Identity: personIdentity(personCode(e.nextPerson)), Phone: "09121234567", Email: "shared@example.com",
		})
		if err != nil {
			t.Fatalf("sign up %s with a shared email: %v", username, err)
		}
	}
	var email string
	if err := e.deps.DB.QueryRow(ctx, `SELECT email FROM users WHERE username = 'second'`).Scan(&email); err != nil || email != "shared@example.com" {
		t.Fatalf("stored email = %q, %v", email, err)
	}

	// Needs a session.
	_, err = e.auth.GetProfile(ctx, &dentalv1.GetProfileRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}
