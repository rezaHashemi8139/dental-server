package service

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	dentalv1 "dental-server/gen/dental/v1"
)

func TestBirthDate(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	inst := e.register(1, "owner")
	e.subscribe("owner", 1, 2, 30)
	ownerLogin, err := e.login(inst, "owner", "Passw0rd-owner")
	if err != nil {
		t.Fatal(err)
	}
	ownerAuth := e.authWith(ownerLogin.res.GetSessionToken())
	owner := e.members(ownerLogin.res.GetSessionToken())

	// The user's own, from the profile.
	updated, err := ownerAuth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{
		FullName: "Dr. owner", Phone: "09121234567", BirthDate: proto.String("1985-07-31"),
	})
	if err != nil || updated.GetProfile().GetBirthDate() != "1985-07-31" {
		t.Fatalf("set birth date: profile = %v, error = %v", updated.GetProfile(), err)
	}
	// An app that does not send it keeps it; an empty one removes it.
	kept, err := ownerAuth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{FullName: "Dr. owner", Phone: "09121234567"})
	if err != nil || kept.GetProfile().GetBirthDate() != "1985-07-31" {
		t.Fatalf("update without birth date: profile = %v, error = %v", kept.GetProfile(), err)
	}
	_, err = ownerAuth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{
		FullName: "Dr. owner", Phone: "09121234567", BirthDate: proto.String("31/07/1985"),
	})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	cleared, err := ownerAuth.UpdateProfile(ctx, &dentalv1.UpdateProfileRequest{
		FullName: "Dr. owner", Phone: "09121234567", BirthDate: proto.String(""),
	})
	if err != nil || cleared.GetProfile().GetBirthDate() != "" {
		t.Fatalf("clear birth date: profile = %v, error = %v", cleared.GetProfile(), err)
	}

	// A staff member's, set by an admin; the member list carries it.
	staff := e.signUp("staff")
	if _, err := owner.AddMember(ctx, &dentalv1.AddMemberRequest{UserId: staff.GetId()}); err != nil {
		t.Fatal(err)
	}
	set, err := owner.SetMemberBirthDate(ctx, &dentalv1.SetMemberBirthDateRequest{UserId: staff.GetId(), BirthDate: "1990-12-22"})
	if err != nil || set.GetMember().GetBirthDate() != "1990-12-22" || set.GetMember().GetUser().GetUsername() != "staff" {
		t.Fatalf("set member birth date = %v, %v", set.GetMember(), err)
	}
	list, err := owner.ListMembers(ctx, &dentalv1.ListMembersRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list.GetMembers() {
		want := map[string]string{"owner": "", "staff": "1990-12-22"}[m.GetUser().GetUsername()]
		if m.GetBirthDate() != want {
			t.Errorf("member %s birth date = %q, want %q", m.GetUser().GetUsername(), m.GetBirthDate(), want)
		}
	}

	// The staff member sees it in their own profile.
	staffLogin, err := e.login(inst, "staff", "Passw0rd-staff")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := e.authWith(staffLogin.res.GetSessionToken()).GetProfile(ctx, &dentalv1.GetProfileRequest{})
	if err != nil || profile.GetProfile().GetBirthDate() != "1990-12-22" {
		t.Fatalf("staff profile = %v, %v", profile.GetProfile(), err)
	}

	// Only admins, and only for members of their clinic.
	_, err = e.members(staffLogin.res.GetSessionToken()).SetMemberBirthDate(ctx,
		&dentalv1.SetMemberBirthDateRequest{UserId: ownerLogin.res.GetUser().GetId(), BirthDate: "1985-07-31"})
	wantCode(t, err, connect.CodePermissionDenied, reasonNotAdmin)
	outsider := e.signUp("outsider")
	_, err = owner.SetMemberBirthDate(ctx, &dentalv1.SetMemberBirthDateRequest{UserId: outsider.GetId(), BirthDate: "1985-07-31"})
	wantCode(t, err, connect.CodeNotFound, reasonUserNotFound)
	_, err = owner.SetMemberBirthDate(ctx, &dentalv1.SetMemberBirthDateRequest{UserId: staff.GetId(), BirthDate: "1990-1-2"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
}
