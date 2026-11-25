package service

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
)

// existingOwnerOf turns registerRequest(machine, ...) into a request by the existing
// owner `owner` (registered with registerRequest(ownerMachine, owner)).
func existingOwnerOf(req *dentalv1.RegisterRequest, owner string, ownerMachine byte) *dentalv1.RegisterRequest {
	req.Owner = &dentalv1.RegisterRequest_ExistingOwner{ExistingOwner: &dentalv1.ExistingOwner{
		Username: owner, Password: "Passw0rd-" + owner, Identity: nationalIdentity(nationalID(int(ownerMachine))),
	}}
	return req
}

func TestAdminPanelSellsSubscriptions(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// The first administrator comes from the command line, with the same rules as the API.
	_, err := AddFirstAdmin(ctx, e.deps.DB, &dentalv1.CreateAdminRequest{Username: "root", Password: "weak", FullName: "Root"})
	wantCode(t, err, connect.CodeInvalidArgument, reasonWeakPassword)
	if _, err := AddFirstAdmin(ctx, e.deps.DB, &dentalv1.CreateAdminRequest{
		Username: "root", Password: "Passw0rd-root", FullName: "Root",
	}); err != nil {
		t.Fatal(err)
	}

	anonymous := e.admin(adminSession{})
	_, err = anonymous.ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
	_, err = anonymous.Login(ctx, &dentalv1.AdminServiceLoginRequest{Username: "root", Password: "wrong"})
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	session, _, err := e.adminLogin("ROOT", "Passw0rd-root")
	if err != nil {
		t.Fatal(err)
	}
	admin := e.admin(session)

	created, err := admin.CreateAdmin(ctx, &dentalv1.CreateAdminRequest{Username: "sales", Password: "Passw0rd-sales", FullName: "Sales"})
	if err != nil || created.GetAdmin().GetUsername() != "sales" {
		t.Fatalf("create admin = %v, %v", created, err)
	}
	_, err = admin.CreateAdmin(ctx, &dentalv1.CreateAdminRequest{Username: "Sales", Password: "Passw0rd-sales", FullName: "Sales"})
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)

	// A clinic owner registers; nobody can log in before a subscription is sold.
	inst := e.register(1, "doctor")
	_, err = e.login(inst, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodePermissionDenied, reasonSubscriptionInactive)

	plan, err := admin.CreatePlan(ctx, &dentalv1.CreatePlanRequest{
		Name: "Basic", MaxClinics: 1, MaxDevicesPerClinic: 2, DurationDays: 3, PriceRials: 10_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: nationalIdentity(nationalID(1))})
	if err != nil || user.GetUser().GetUsername() != "doctor" || len(user.GetOwnedClinics()) != 1 ||
		user.GetOwnedClinics()[0].GetInstallationCount() != 1 || user.GetIdentity().GetNationality() != "IR" {
		t.Fatalf("get user = %v, %v", user, err)
	}
	_, err = admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: nationalIdentity(nationalID(77))})
	wantCode(t, err, connect.CodeNotFound, reasonUserNotFound)

	// Two sales: the second starts when the first ends.
	sell := func() (*dentalv1.Subscription, error) {
		res, err := admin.SellSubscription(ctx, &dentalv1.SellSubscriptionRequest{
			UserId: user.GetUser().GetId(), PlanId: plan.GetPlan().GetId(), Note: "receipt 42",
		})
		return res.GetSubscription(), err
	}
	first, err := sell()
	if err != nil {
		t.Fatal(err)
	}
	second, err := sell()
	if err != nil {
		t.Fatal(err)
	}
	if !second.GetStartsAt().AsTime().Equal(first.GetEndsAt().AsTime()) {
		t.Errorf("second starts %v, want %v", second.GetStartsAt().AsTime(), first.GetEndsAt().AsTime())
	}

	// Login works; paid time and offline access end with the second subscription (6 days, under the 7 offline days).
	res, err := e.login(inst, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	paidEnd := second.GetEndsAt().AsTime()
	if !res.res.GetSubscriptionEndsAt().AsTime().Equal(paidEnd) || !res.grant.GetExpiresAt().AsTime().Equal(paidEnd) {
		t.Errorf("subscription ends %v, grant expires %v, want both %v",
			res.res.GetSubscriptionEndsAt().AsTime(), res.grant.GetExpiresAt().AsTime(), paidEnd)
	}

	// An admin session is not a user session, and the other way round.
	_, err = e.members(session.token).ListMembers(ctx, &dentalv1.ListMembersRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
	_, err = e.admin(adminSession{token: res.res.GetSessionToken(), csrf: session.csrf}).ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)

	// Canceling the subscription in force ends access at once: the renewal has not started yet.
	canceled, err := admin.CancelSubscription(ctx, &dentalv1.CancelSubscriptionRequest{Id: first.GetId()})
	if err != nil || canceled.GetSubscription().GetCanceledAt() == nil {
		t.Fatalf("cancel = %v, %v", canceled, err)
	}
	_, err = e.members(res.res.GetSessionToken()).ListMembers(ctx, &dentalv1.ListMembersRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
	_, err = e.login(inst, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodePermissionDenied, reasonSubscriptionInactive)
	_, err = admin.CancelSubscription(ctx, &dentalv1.CancelSubscriptionRequest{Id: bytes.Repeat([]byte{3}, 16)})
	wantCode(t, err, connect.CodeNotFound, reasonSubscriptionNotFound)

	// A new sale starts after the paid time still left: the queued renewal, not the canceled one.
	third, err := sell()
	if err != nil {
		t.Fatal(err)
	}
	if !third.GetStartsAt().AsTime().Equal(second.GetEndsAt().AsTime()) {
		t.Errorf("third starts %v, want after the queued renewal %v", third.GetStartsAt().AsTime(), second.GetEndsAt().AsTime())
	}

	// Inactive plans cannot be sold; plan changes do not touch subscriptions already sold.
	updated, err := admin.UpdatePlan(ctx, &dentalv1.UpdatePlanRequest{
		Id: plan.GetPlan().GetId(), Name: "Basic (old)", MaxClinics: 5, MaxDevicesPerClinic: 5, DurationDays: 30, Active: false,
	})
	if err != nil || updated.GetPlan().GetActive() {
		t.Fatalf("update plan = %v, %v", updated, err)
	}
	_, err = sell()
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPlanInactive)
	user, err = admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: nationalIdentity(nationalID(1))})
	if err != nil || len(user.GetSubscriptions()) != 3 || user.GetSubscriptions()[0].GetMaxClinics() != 1 {
		t.Fatalf("subscriptions after plan change = %v, %v", user.GetSubscriptions(), err)
	}

	if _, err := admin.Logout(ctx, &dentalv1.AdminServiceLogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	_, err = admin.ListPlans(ctx, &dentalv1.ListPlansRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}

func TestSubscriptionLimits(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// Clinic A with two installations, then clinic B; the subscription covers one clinic with one installation.
	a1 := e.register(1, "doctor")
	a2, err := e.installation.Register(ctx, existingOwnerOf(&dentalv1.RegisterRequest{
		Machine: registerRequest(2, "doctor").GetMachine(),
		Target:  &dentalv1.RegisterRequest_ExistingClinic{ExistingClinic: &dentalv1.ExistingClinic{LicenseNumber: "lic-1"}},
	}, "doctor", 1))
	if err != nil || !bytes.Equal(a2.GetClinicId(), a1.GetClinicId()) {
		t.Fatalf("second installation of clinic A = %v, %v", a2, err)
	}
	b, err := e.installation.Register(ctx, existingOwnerOf(registerRequest(3, "unused"), "doctor", 1))
	if err != nil {
		t.Fatal(err)
	}
	e.subscribe("doctor", 1, 1, 30)

	if _, err := e.login(a1, "doctor", "Passw0rd-doctor"); err != nil {
		t.Fatal(err)
	}
	_, err = e.login(a2, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodePermissionDenied, reasonDeviceLimitExceeded)
	_, err = e.login(b, "doctor", "Passw0rd-doctor")
	wantCode(t, err, connect.CodePermissionDenied, reasonClinicLimitExceeded)

	// Revoking the first installation frees its place.
	installationID, _ := toUUID(a1.GetInstallationId())
	if _, err := e.deps.DB.Exec(ctx, `UPDATE installations SET revoked_at = now() WHERE id = $1`, installationID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.login(a2, "doctor", "Passw0rd-doctor"); err != nil {
		t.Fatalf("login after revoking the other installation: %v", err)
	}

	// Only the owner can add installations to a clinic, and only to an existing one.
	e.register(4, "dentist")
	toExisting := func(license string, owner string, ownerMachine byte) error {
		_, err := e.installation.Register(ctx, existingOwnerOf(&dentalv1.RegisterRequest{
			Machine: registerRequest(5, owner).GetMachine(),
			Target:  &dentalv1.RegisterRequest_ExistingClinic{ExistingClinic: &dentalv1.ExistingClinic{LicenseNumber: license}},
		}, owner, ownerMachine))
		return err
	}
	wantCode(t, toExisting("LIC-1", "dentist", 4), connect.CodePermissionDenied, reasonNotClinicOwner)
	wantCode(t, toExisting("LIC-404", "dentist", 4), connect.CodeNotFound, reasonClinicNotFound)
	newOwner := registerRequest(5, "surgeon")
	newOwner.Target = &dentalv1.RegisterRequest_ExistingClinic{ExistingClinic: &dentalv1.ExistingClinic{LicenseNumber: "LIC-1"}}
	_, err = e.installation.Register(ctx, newOwner)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
}

func TestClinicCreatesAccounts(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	instA := e.register(1, "doctor")
	instB := e.register(2, "dentist")
	e.subscribe("doctor", 1, 2, 30)
	e.subscribe("dentist", 1, 1, 30)
	loginA, err := e.login(instA, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	loginB, err := e.login(instB, "dentist", "Passw0rd-dentist")
	if err != nil {
		t.Fatal(err)
	}
	clinicA, clinicB := e.members(loginA.res.GetSessionToken()), e.members(loginB.res.GetSessionToken())

	// Clinic A creates an account for a hygienist; clinic B asks for the same person and gets the same account.
	request := &dentalv1.CreateUserRequest{
		Identity: nationalIdentity(nationalID(500)), Username: "hygienist", TemporaryPassword: "Temp-passw0rd",
		FullName: "Hygienist", Phone: "09121234567",
	}
	created, err := clinicA.CreateUser(ctx, request)
	if err != nil || !created.GetCreated() {
		t.Fatalf("create user = %v, %v", created, err)
	}
	request.Username, request.TemporaryPassword = "other-name", "Other-passw0rd"
	again, err := clinicB.CreateUser(ctx, request)
	if err != nil || again.GetCreated() || !bytes.Equal(again.GetUser().GetId(), created.GetUser().GetId()) ||
		again.GetUser().GetUsername() != "hygienist" {
		t.Fatalf("create the same person again = %v, %v", again, err)
	}
	request.Identity, request.Username = nationalIdentity(nationalID(501)), "Doctor"
	_, err = clinicB.CreateUser(ctx, request)
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)
	request.Identity = nationalIdentity("0013542418")
	_, err = clinicB.CreateUser(ctx, request)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidNationalID)

	// A member of clinic A, with a temporary password that must be changed first.
	if _, err := clinicA.AddMember(ctx, &dentalv1.AddMemberRequest{UserId: created.GetUser().GetId()}); err != nil {
		t.Fatal(err)
	}
	_, err = e.login(instA, "hygienist", "Temp-passw0rd")
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPasswordChangeRequired)
	change := func(current, next string) error {
		_, err := e.auth.ChangePassword(ctx, &dentalv1.ChangePasswordRequest{
			Username: "hygienist", CurrentPassword: current, NewPassword: next,
		})
		return err
	}
	wantCode(t, change("wrong", "New-passw0rd"), connect.CodeUnauthenticated, reasonInvalidCredentials)
	wantCode(t, change("Temp-passw0rd", "Temp-passw0rd"), connect.CodeInvalidArgument, reasonPasswordUnchanged)
	wantCode(t, change("Temp-passw0rd", "weak"), connect.CodeInvalidArgument, reasonWeakPassword)
	if err := change("Temp-passw0rd", "New-passw0rd"); err != nil {
		t.Fatal(err)
	}

	// Membership is per clinic: every installation of clinic A, none of clinic B.
	a2, err := e.installation.Register(ctx, existingOwnerOf(&dentalv1.RegisterRequest{
		Machine: registerRequest(3, "doctor").GetMachine(),
		Target:  &dentalv1.RegisterRequest_ExistingClinic{ExistingClinic: &dentalv1.ExistingClinic{LicenseNumber: "LIC-1"}},
	}, "doctor", 1))
	if err != nil {
		t.Fatal(err)
	}
	var session string
	for _, inst := range []*dentalv1.RegisterResponse{instA, a2} {
		res, err := e.login(inst, "hygienist", "New-passw0rd")
		if err != nil {
			t.Fatal(err)
		}
		session = res.res.GetSessionToken()
	}
	_, err = e.login(instB, "hygienist", "New-passw0rd")
	wantCode(t, err, connect.CodePermissionDenied, reasonNotAMember)

	// Changing the password ends every session of the account.
	if err := change("New-passw0rd", "Newer-passw0rd"); err != nil {
		t.Fatal(err)
	}
	_, err = e.members(session).ListMembers(ctx, &dentalv1.ListMembersRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}

func TestForeignNationals(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	foreign := func(number, nationality string) *dentalv1.PersonIdentity {
		return &dentalv1.PersonIdentity{Type: dentalv1.IdentityType_IDENTITY_TYPE_FOREIGN, Number: number, Nationality: nationality}
	}
	signUp := func(username string, identity *dentalv1.PersonIdentity) (*dentalv1.SignUpResponse, error) {
		return e.auth.SignUp(ctx, &dentalv1.SignUpRequest{
			Username: username, Password: "Passw0rd-" + username, FullName: "Someone", Identity: identity, Phone: "09121234567",
		})
	}

	// A foreign national signs up with their FIDA code; it is unique like a national ID.
	if _, err := signUp("assistant", foreign("123456789012", "AF")); err != nil {
		t.Fatal(err)
	}
	_, err := signUp("assistant2", foreign("123456789012", "IQ"))
	wantCode(t, err, connect.CodeAlreadyExists, reasonIdentityTaken)

	// The nationality must fit the type of identity.
	_, err = signUp("assistant3", foreign("123456789013", "IR"))
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	wrongNationality := nationalIdentity(nationalID(9))
	wrongNationality.Nationality = "AF"
	_, err = signUp("assistant3", wrongNationality)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	_, err = signUp("assistant3", foreign("123456789013", "af"))
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// A foreign clinic owner, and a clinic finding a foreign national's account.
	req := registerRequest(1, "owner")
	req.GetNewOwner().Identity = foreign("ABC12345", "TR")
	inst, err := e.installation.Register(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	e.subscribe("owner", 1, 1, 30)
	login, err := e.login(inst, "owner", "Passw0rd-owner")
	if err != nil {
		t.Fatal(err)
	}
	found, err := e.members(login.res.GetSessionToken()).FindUser(ctx, &dentalv1.FindUserRequest{
		Identity: foreign("123456789012", "AF"),
	})
	if err != nil || found.GetUser().GetUsername() != "assistant" {
		t.Fatalf("find foreign national = %v, %v", found, err)
	}

	var identityType, nationality string
	if err := e.deps.DB.QueryRow(ctx, `SELECT identity_type, nationality FROM users WHERE username = 'owner'`).
		Scan(&identityType, &nationality); err != nil || identityType != "foreign" || nationality != "TR" {
		t.Fatalf("stored owner identity = %q %q, %v", identityType, nationality, err)
	}

	// The existing-owner check compares the identity, not only the number.
	again := existingOwnerOf(registerRequest(2, "unused"), "owner", 1)
	again.GetExistingOwner().Identity = foreign("ABC12345", "TR")
	if _, err := e.installation.Register(ctx, again); err != nil {
		t.Fatal(err)
	}
	again = existingOwnerOf(registerRequest(3, "unused"), "owner", 1)
	again.GetExistingOwner().Identity = foreign("ABC99999", "TR")
	_, err = e.installation.Register(ctx, again)
	wantCode(t, err, connect.CodeFailedPrecondition, reasonIdentityMismatch)
}
