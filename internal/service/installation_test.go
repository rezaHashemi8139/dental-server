package service

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/internal/secure"
)

func TestRegisterClinic(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	first := e.register(1, "doctor")
	if first.GetOwner().GetUsername() != "doctor" || len(first.GetOwner().GetId()) != 16 {
		t.Fatalf("owner = %v", first.GetOwner())
	}

	// The license number is unique, not case-sensitive. A failed registration leaves
	// nothing behind: the new owner's username and identification code stay free.
	dup := registerRequest(2, "dentist")
	dup.GetClinic().LicenseNumber = "lic-1"
	_, err := e.installation.Register(ctx, dup)
	wantCode(t, err, connect.CodeAlreadyExists, reasonLicenseNumberTaken)
	e.register(2, "dentist")

	// A new owner's username and identification code must be free.
	taken := registerRequest(3, "Doctor")
	_, err = e.installation.Register(ctx, taken)
	wantCode(t, err, connect.CodeAlreadyExists, reasonUsernameTaken)
	taken = registerRequest(3, "surgeon")
	taken.GetNewOwner().Identity = personIdentity(personCode(1))
	_, err = e.installation.Register(ctx, taken)
	wantCode(t, err, connect.CodeAlreadyExists, reasonIdentityTaken)

	// The identification code needs uppercase letters or digits and is always required.
	bad := registerRequest(3, "surgeon")
	bad.GetNewOwner().Identity = personIdentity("abc12345")
	_, err = e.installation.Register(ctx, bad)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
	bad.GetNewOwner().Identity = nil
	_, err = e.installation.Register(ctx, bad)
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// An existing owner: the doctor opens a second clinic.
	existing := func(machine byte, username, password, code string) *dentalv1.RegisterRequest {
		req := registerRequest(machine, "unused")
		req.Owner = &dentalv1.RegisterRequest_ExistingOwner{ExistingOwner: &dentalv1.ExistingOwner{
			Username: username, Password: password, Identity: personIdentity(code),
		}}
		return req
	}
	_, err = e.installation.Register(ctx, existing(4, "doctor", "wrong-password", personCode(1)))
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	_, err = e.installation.Register(ctx, existing(4, "nobody", "Passw0rd-nobody", personCode(1)))
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	_, err = e.installation.Register(ctx, existing(4, "doctor", "Passw0rd-doctor", personCode(2)))
	wantCode(t, err, connect.CodeFailedPrecondition, reasonIdentityMismatch)

	second, err := e.installation.Register(ctx, existing(4, "DOCTOR", "Passw0rd-doctor", personCode(1)))
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

	// An owner with an identity on file may leave it out (they logged in first, see ListMyClinics).
	withoutIdentity := existing(7, "doctor", "Passw0rd-doctor", "")
	withoutIdentity.GetExistingOwner().Identity = nil
	if _, err := e.installation.Register(ctx, withoutIdentity); err != nil {
		t.Fatal(err)
	}

	// An account made before identities were required has none: it must be sent,
	// the first clinic saves it, and it must then not belong to another account.
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO users (username, password_hash, full_name) VALUES ('hygienist', $1, 'Old account')`,
		secure.HashPassword("Passw0rd-hygienist")); err != nil {
		t.Fatal(err)
	}
	legacy := existing(5, "hygienist", "Passw0rd-hygienist", "")
	legacy.GetExistingOwner().Identity = nil
	_, err = e.installation.Register(ctx, legacy)
	wantCode(t, err, connect.CodeFailedPrecondition, reasonIdentityRequired)
	_, err = e.installation.Register(ctx, existing(5, "hygienist", "Passw0rd-hygienist", personCode(2)))
	wantCode(t, err, connect.CodeAlreadyExists, reasonIdentityTaken)
	if _, err := e.installation.Register(ctx, existing(5, "hygienist", "Passw0rd-hygienist", personCode(5))); err != nil {
		t.Fatal(err)
	}
	var saved string
	if err := e.deps.DB.QueryRow(ctx, `SELECT identity FROM users WHERE username = 'hygienist'`).Scan(&saved); err != nil || saved != personCode(5) {
		t.Fatalf("saved identification code = %q, %v", saved, err)
	}

	// An account with a temporary password cannot own a new clinic until it is changed.
	if _, err := e.deps.DB.Exec(ctx, `UPDATE users SET must_change_password = true WHERE username = 'hygienist'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.installation.Register(ctx, existing(6, "hygienist", "Passw0rd-hygienist", personCode(5)))
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPasswordChangeRequired)

	// A disabled account cannot own a new clinic.
	if _, err := e.deps.DB.Exec(ctx, `
		UPDATE users SET must_change_password = false, disabled_at = now() WHERE username = 'hygienist'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.installation.Register(ctx, existing(6, "hygienist", "Passw0rd-hygienist", personCode(5)))
	wantCode(t, err, connect.CodePermissionDenied, reasonUserDisabled)
}

func TestListMyClinics(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	list := func(username string) *dentalv1.ListMyClinicsResponse {
		t.Helper()
		res, err := e.installation.ListMyClinics(ctx, &dentalv1.ListMyClinicsRequest{
			Username: username, Password: "Passw0rd-" + username,
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	reasons := func(res *dentalv1.ListMyClinicsResponse) []string {
		var got []string
		for _, c := range res.GetClinics() {
			got = append(got, c.GetLicenseNumber()+":"+c.GetCannotAddReason())
		}
		return got
	}
	want := func(res *dentalv1.ListMyClinicsResponse, wantReasons ...string) {
		t.Helper()
		if got := reasons(res); !slices.Equal(got, wantReasons) {
			t.Errorf("clinics = %q, want %q", got, wantReasons)
		}
	}

	// A new account has no clinic yet: the app offers to add one. No login session needed.
	e.signUp("nurse")
	nurse := list("nurse")
	if nurse.GetUser().GetUsername() != "nurse" || !nurse.GetIdentityOnFile() || len(nurse.GetClinics()) != 0 ||
		nurse.GetSubscriptionEndsAt() != nil || nurse.GetMaxClinics() != 0 || nurse.GetOwnedClinicCount() != 0 {
		t.Fatalf("new account = %v", nurse)
	}
	_, err := e.installation.ListMyClinics(ctx, &dentalv1.ListMyClinicsRequest{Username: "nurse", Password: "wrong"})
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)
	_, err = e.installation.ListMyClinics(ctx, &dentalv1.ListMyClinicsRequest{Username: "nobody", Password: "wrong"})
	wantCode(t, err, connect.CodeUnauthenticated, reasonInvalidCredentials)

	// Without a subscription, nothing can be added.
	e.register(1, "doctor")
	want(list("doctor"), "LIC-1:subscription_inactive")

	// One clinic with one computer allowed, already used.
	e.subscribe("doctor", 1, 1, 30)
	doctor := list("doctor")
	want(doctor, "LIC-1:device_limit_exceeded")
	c := doctor.GetClinics()[0]
	if !c.GetIsOwner() || c.GetOwnerFullName() != "Dr. doctor" || c.GetInstallationCount() != 1 ||
		c.GetMaxInstallations() != 1 || c.GetSubscriptionEndsAt() == nil || c.GetName() != "Clinic doctor" ||
		c.GetCity() == "" || c.GetCityId() != 1 || c.GetAddress() != "1 Test Street" || len(c.GetId()) != 16 {
		t.Fatalf("clinic = %v", c)
	}

	// A second clinic is beyond the clinic limit; the oldest one stays covered.
	second := registerRequest(2, "unused")
	second.Owner = &dentalv1.RegisterRequest_ExistingOwner{ExistingOwner: &dentalv1.ExistingOwner{
		Username: "doctor", Password: "Passw0rd-doctor",
	}}
	if _, err := e.installation.Register(ctx, second); err != nil {
		t.Fatal(err)
	}
	doctor = list("doctor")
	want(doctor, "LIC-1:device_limit_exceeded", "LIC-2:clinic_limit_exceeded")
	if doctor.GetMaxClinics() != 1 || doctor.GetOwnedClinicCount() != 2 || doctor.GetSubscriptionEndsAt() == nil {
		t.Errorf("own subscription = %d clinics, %d owned, ends %v",
			doctor.GetMaxClinics(), doctor.GetOwnedClinicCount(), doctor.GetSubscriptionEndsAt())
	}

	// The doctor is an admin of the dentist's clinic and a plain member of the surgeon's:
	// clinics they own come first, the admin one cannot be picked, the member one is left out.
	e.register(3, "dentist")
	e.subscribe("dentist", 1, 2, 30)
	e.register(4, "surgeon")
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO clinic_members (clinic_id, user_id, is_admin)
		SELECT c.id, u.id, c.license_number = 'LIC-3' FROM clinics c, users u
		WHERE c.license_number IN ('LIC-3', 'LIC-4') AND u.username = 'doctor'`); err != nil {
		t.Fatal(err)
	}
	doctor = list("doctor")
	want(doctor, "LIC-1:device_limit_exceeded", "LIC-2:clinic_limit_exceeded", "LIC-3:not_clinic_owner")
	if c := doctor.GetClinics()[2]; c.GetIsOwner() || c.GetOwnerFullName() != "Dr. dentist" || c.GetMaxInstallations() != 2 {
		t.Errorf("admin clinic = %v", c)
	}
	want(list("dentist"), "LIC-3:")

	// Same account checks as Login.
	if _, err := e.deps.DB.Exec(ctx, `UPDATE users SET must_change_password = true WHERE username = 'dentist'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.installation.ListMyClinics(ctx, &dentalv1.ListMyClinicsRequest{Username: "dentist", Password: "Passw0rd-dentist"})
	wantCode(t, err, connect.CodeFailedPrecondition, reasonPasswordChangeRequired)
	if _, err := e.deps.DB.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE username = 'surgeon'`); err != nil {
		t.Fatal(err)
	}
	_, err = e.installation.ListMyClinics(ctx, &dentalv1.ListMyClinicsRequest{Username: "surgeon", Password: "Passw0rd-surgeon"})
	wantCode(t, err, connect.CodePermissionDenied, reasonUserDisabled)
}

func TestListCities(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// No login needed: the clinic form is shown before the computer is registered.
	res, err := e.installation.ListCities(ctx, &dentalv1.ListCitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	cities := map[uint32]bool{}
	for _, p := range res.GetProvinces() {
		if p.GetId() == 0 || p.GetName() == "" || len(p.GetCities()) == 0 {
			t.Errorf("province = %v", p)
		}
		for _, c := range p.GetCities() {
			if c.GetId() == 0 || c.GetName() == "" || cities[c.GetId()] {
				t.Errorf("city = %v", c)
			}
			cities[c.GetId()] = true
		}
	}
	if len(res.GetProvinces()) != 31 || len(cities) != 1242 {
		t.Errorf("%d provinces, %d cities; want 31 and 1242", len(res.GetProvinces()), len(cities))
	}

	// A clinic is registered in a city of the list; its province and name are stored with it.
	unknown := registerRequest(1, "doctor")
	unknown.GetClinic().CityId = 99999
	_, err = e.installation.Register(ctx, unknown)
	wantCode(t, err, connect.CodeNotFound, reasonCityNotFound)
	e.register(1, "doctor")
	var cityID int32
	var sameProvince, sameName bool
	if err := e.deps.DB.QueryRow(ctx, `
		SELECT c.city_id, c.province_id = ci.province_id, c.city = ci.name
		FROM clinics c JOIN cities ci ON ci.id = c.city_id`,
	).Scan(&cityID, &sameProvince, &sameName); err != nil || cityID != 1 || !sameProvince || !sameName {
		t.Fatalf("clinic city %d: same province %v, same name %v, %v", cityID, sameProvince, sameName, err)
	}
}
