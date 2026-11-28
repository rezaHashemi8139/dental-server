package service

import (
	"bytes"
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
)

// existingOwnerOf turns registerRequest(machine, ...) into a request by the existing
// owner `owner` (registered with registerRequest(ownerMachine, owner)).
func existingOwnerOf(req *dentalv1.RegisterRequest, owner string, ownerMachine byte) *dentalv1.RegisterRequest {
	req.Owner = &dentalv1.RegisterRequest_ExistingOwner{ExistingOwner: &dentalv1.ExistingOwner{
		Username: owner, Password: "Passw0rd-" + owner, Identity: personIdentity(personCode(int(ownerMachine))),
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
	user, err := admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: personIdentity(personCode(1))})
	if err != nil || user.GetUser().GetUsername() != "doctor" || len(user.GetOwnedClinics()) != 1 ||
		user.GetOwnedClinics()[0].GetInstallationCount() != 1 || user.GetIdentity().GetNumber() != personCode(1) {
		t.Fatalf("get user = %v, %v", user, err)
	}
	_, err = admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: personIdentity(personCode(77))})
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
	user, err = admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: personIdentity(personCode(1))})
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
		Identity: personIdentity(personCode(500)), Username: "hygienist", TemporaryPassword: "Temp-passw0rd",
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
	request.Identity, request.Username = personIdentity(personCode(501)), "Doctor"
	_, err = clinicB.CreateUser(ctx, request)
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)
	request.Identity = personIdentity("12")
	_, err = clinicB.CreateUser(ctx, request)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

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

func TestIdentificationCodes(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	signUp := func(username, code string) (*dentalv1.SignUpResponse, error) {
		return e.auth.SignUp(ctx, &dentalv1.SignUpRequest{
			Username: username, Password: "Passw0rd-" + username, FullName: "Someone",
			Identity: personIdentity(code), Phone: "09121234567",
		})
	}

	// Any code of 5–20 uppercase letters or digits is taken; it is unique across all accounts.
	if _, err := signUp("assistant", "123456789012"); err != nil {
		t.Fatal(err)
	}
	_, err := signUp("assistant2", "123456789012")
	wantCode(t, err, connect.CodeAlreadyExists, reasonIdentityTaken)
	for _, code := range []string{"abc12345", "1234", "123456789012345678901", "AB-12345"} {
		_, err = signUp("assistant3", code)
		wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	// A clinic owner with a code of letters and digits, and a clinic finding an account by code.
	req := registerRequest(1, "owner")
	req.GetNewOwner().Identity = personIdentity("ABC12345")
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
		Identity: personIdentity("123456789012"),
	})
	if err != nil || found.GetUser().GetUsername() != "assistant" {
		t.Fatalf("find by code = %v, %v", found, err)
	}

	var stored string
	if err := e.deps.DB.QueryRow(ctx, `SELECT identity FROM users WHERE username = 'owner'`).
		Scan(&stored); err != nil || stored != "ABC12345" {
		t.Fatalf("stored owner identity = %q, %v", stored, err)
	}

	// The existing-owner check compares the code.
	again := existingOwnerOf(registerRequest(2, "unused"), "owner", 1)
	again.GetExistingOwner().Identity = personIdentity("ABC12345")
	if _, err := e.installation.Register(ctx, again); err != nil {
		t.Fatal(err)
	}
	again = existingOwnerOf(registerRequest(3, "unused"), "owner", 1)
	again.GetExistingOwner().Identity = personIdentity("ABC99999")
	_, err = e.installation.Register(ctx, again)
	wantCode(t, err, connect.CodeFailedPrecondition, reasonIdentityMismatch)
}

func TestAdminGetsSubscription(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	if _, err := AddFirstAdmin(ctx, e.deps.DB, &dentalv1.CreateAdminRequest{
		Username: "root", Password: "Passw0rd-root", FullName: "Root",
	}); err != nil {
		t.Fatal(err)
	}
	session, _, err := e.adminLogin("root", "Passw0rd-root")
	if err != nil {
		t.Fatal(err)
	}
	admin := e.admin(session)

	// Clinic A with three installations (the last one revoked), then clinic B with one.
	a1 := e.register(1, "doctor")
	toClinicA := func(machine byte) *dentalv1.RegisterResponse {
		t.Helper()
		res, err := e.installation.Register(ctx, existingOwnerOf(&dentalv1.RegisterRequest{
			Machine: registerRequest(machine, "doctor").GetMachine(),
			Target:  &dentalv1.RegisterRequest_ExistingClinic{ExistingClinic: &dentalv1.ExistingClinic{LicenseNumber: "LIC-1"}},
		}, "doctor", 1))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a2, a3 := toClinicA(2), toClinicA(3)
	b, err := e.installation.Register(ctx, existingOwnerOf(registerRequest(4, "unused"), "doctor", 1))
	if err != nil {
		t.Fatal(err)
	}
	revokedID, _ := toUUID(a3.GetInstallationId())
	if _, err := e.deps.DB.Exec(ctx, `UPDATE installations SET revoked_at = now() WHERE id = $1`, revokedID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deps.DB.Exec(ctx, `UPDATE users SET email = 'doctor@clinic.example' WHERE username = 'doctor'`); err != nil {
		t.Fatal(err)
	}

	// One clinic with one installation: clinic B and the second installation of A are beyond it.
	plan, err := admin.CreatePlan(ctx, &dentalv1.CreatePlanRequest{
		Name: "Basic", MaxClinics: 1, MaxDevicesPerClinic: 1, DurationDays: 30, PriceRials: 10_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	user, err := admin.GetUser(ctx, &dentalv1.GetUserRequest{Identity: personIdentity(personCode(1))})
	if err != nil {
		t.Fatal(err)
	}
	sold, err := admin.SellSubscription(ctx, &dentalv1.SellSubscriptionRequest{
		UserId: user.GetUser().GetId(), PlanId: plan.GetPlan().GetId(), Note: "receipt 7",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The doctor logs in on the first installation and syncs a patient.
	login, err := e.login(a1, "doctor", "Passw0rd-doctor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.sync(login.res.GetSessionToken()).Push(ctx, &dentalv1.PushRequest{
		Patients: []*dentalv1.Patient{testPatient(personCode(5000), "Sara", time.Now())},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := admin.GetSubscription(ctx, &dentalv1.GetSubscriptionRequest{Id: sold.GetSubscription().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	item := res.GetItem()
	if !bytes.Equal(item.GetSubscription().GetId(), sold.GetSubscription().GetId()) ||
		item.GetSubscription().GetNote() != "receipt 7" ||
		item.GetStatus() != dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE ||
		item.GetUser().GetUsername() != "doctor" || item.GetIdentity().GetNumber() != personCode(1) ||
		item.GetPhone() == "" || item.GetUserDisabled() || item.GetPurchase() != nil || len(item.GetOwnedClinics()) != 2 {
		t.Errorf("item = %v", item)
	}
	if res.GetEmail() != "doctor@clinic.example" {
		t.Errorf("email = %q", res.GetEmail())
	}

	// The clinics oldest first: A, then B beyond the clinic limit.
	if len(res.GetClinics()) != 2 {
		t.Fatalf("clinics = %v", res.GetClinics())
	}
	clinicA, clinicB := res.GetClinics()[0], res.GetClinics()[1]
	if !bytes.Equal(clinicA.GetId(), a1.GetClinicId()) || clinicA.GetName() != "Clinic doctor" ||
		clinicA.GetLicenseNumber() != "LIC-1" || clinicA.GetPhone() != "02112345678" ||
		clinicA.GetProvince() == "" || clinicA.GetCity() == "" || clinicA.GetAddress() != "1 Test Street" ||
		clinicA.GetLocation().GetLatitude() != 35.6892 || clinicA.GetLocation().GetLongitude() != 51.3890 ||
		clinicA.GetCreatedAt() == nil || clinicA.GetUpdatedAt() == nil ||
		clinicA.GetMemberCount() != 1 || clinicA.GetPatientCount() != 1 || clinicA.GetOverClinicLimit() {
		t.Errorf("clinic A = %v", clinicA)
	}
	if !bytes.Equal(clinicB.GetId(), b.GetClinicId()) || !clinicB.GetOverClinicLimit() ||
		clinicB.GetPatientCount() != 0 || len(clinicB.GetInstallations()) != 1 ||
		clinicB.GetInstallations()[0].GetOverDeviceLimit() {
		t.Errorf("clinic B = %v", clinicB)
	}

	// A's installations: not revoked newest first, then the revoked one. Only the oldest
	// one that is not revoked is within the device limit.
	installations := clinicA.GetInstallations()
	if len(installations) != 3 {
		t.Fatalf("installations of A = %v", installations)
	}
	for i, want := range []struct {
		inst       *dentalv1.RegisterResponse
		overLimit  bool
		revoked    bool
		loggedInBy string
	}{
		{a2, true, false, ""},
		{a1, false, false, "doctor"},
		{a3, false, true, ""},
	} {
		got := installations[i]
		if !bytes.Equal(got.GetId(), want.inst.GetInstallationId()) || got.GetOverDeviceLimit() != want.overLimit ||
			(got.GetRevokedAt() != nil) != want.revoked || got.GetLastLoginUser().GetUsername() != want.loggedInBy ||
			(got.GetLastLoginAt() != nil) != (want.loggedInBy != "") {
			t.Errorf("installation %d = %v", i, got)
		}
	}
	first := installations[1]
	if !bytes.Equal(first.GetMachineIdHash(), bytes.Repeat([]byte{1}, 32)) || first.GetHostname() != "clinic-pc" ||
		len(first.GetMacAddresses()) != 1 || first.GetMacAddresses()[0] != "AA:BB:CC:DD:EE:FF" ||
		first.GetOfflineAccess().AsDuration() != 7*24*time.Hour || first.GetCreatedAt() == nil ||
		first.GetLastSeenAt() == nil || len(first.GetLastLoginUser().GetId()) != 16 {
		t.Errorf("first installation = %v", first)
	}

	_, err = admin.GetSubscription(ctx, &dentalv1.GetSubscriptionRequest{Id: bytes.Repeat([]byte{3}, 16)})
	wantCode(t, err, connect.CodeNotFound, reasonSubscriptionNotFound)
	_, err = admin.GetSubscription(ctx, &dentalv1.GetSubscriptionRequest{Id: []byte{1}})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	_, err = e.admin(adminSession{}).GetSubscription(ctx, &dentalv1.GetSubscriptionRequest{Id: sold.GetSubscription().GetId()})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}
