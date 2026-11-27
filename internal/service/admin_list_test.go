package service

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
)

func TestAdminListsPages(t *testing.T) {
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

	alice, bob := e.signUp("alice"), e.signUp("bob")
	var planID [16]byte
	if err := e.deps.DB.QueryRow(ctx, `
		INSERT INTO plans (name, max_clinics, max_devices_per_clinic, duration_days)
		VALUES ('Basic', 1, 1, 30) RETURNING id`).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	// alice: one ended, one active (bought with a payment), one upcoming; bob: one canceled.
	subscription := func(user *dentalv1.User, startDays, endDays int, canceled bool) [16]byte {
		t.Helper()
		var id [16]byte
		if err := e.deps.DB.QueryRow(ctx, `
			INSERT INTO subscriptions (user_id, plan_id, max_clinics, max_devices_per_clinic, price_rials,
			                           starts_at, ends_at, canceled_at)
			VALUES ($1, $2, 1, 1, 0, now() + make_interval(days => $3), now() + make_interval(days => $4),
			        CASE WHEN $5 THEN now() END)
			RETURNING id`, user.GetId(), planID, startDays, endDays, canceled).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	ended := subscription(alice, -60, -30, false)
	active := subscription(alice, -30, 1, false)
	upcoming := subscription(alice, 1, 31, false)
	canceled := subscription(bob, -10, 20, true)
	var accountID [16]byte
	if err := e.deps.DB.QueryRow(ctx, `
		INSERT INTO payment_accounts (bank_name, holder_name, card_number) VALUES ('Bank', 'Dental', '1234567812345678')
		RETURNING id`).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deps.DB.Exec(ctx, `
		INSERT INTO payments (user_id, plan_id, max_clinics, max_devices_per_clinic, duration_days, mode, method,
		                      status, price_rials, amount_rials, reference, subscription_id, created_at, payment_account_id)
		VALUES ($1, $2, 1, 1, 30, 'extend', 'card_to_card', 'approved', 5, 5, 'REF-ALICE', $3, now() - interval '2 days', $5),
		       ($4, $2, 1, 1, 30, 'extend', 'card_to_card', 'rejected', 5, 5, 'REF-BOB-1', NULL, now() - interval '1 day', $5),
		       ($4, $2, 1, 1, 30, 'extend', 'card_to_card', 'pending', 5, 5, 'REF-BOB-2', NULL, now(), $5)`,
		alice.GetId(), planID, active, bob.GetId(), accountID); err != nil {
		t.Fatal(err)
	}

	list := func(req *dentalv1.ListSubscriptionsRequest) *dentalv1.ListSubscriptionsResponse {
		t.Helper()
		res, err := admin.ListSubscriptions(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Newest first, a page at a time, with the total on every page.
	var got [][16]byte
	for page := uint32(1); page <= 3; page++ {
		res := list(&dentalv1.ListSubscriptionsRequest{Page: page, PageSize: 2})
		if res.GetTotalCount() != 4 {
			t.Fatalf("page %d total = %d, want 4", page, res.GetTotalCount())
		}
		for _, item := range res.GetItems() {
			got = append(got, [16]byte(item.GetSubscription().GetId()))
		}
	}
	if want := [][16]byte{upcoming, canceled, active, ended}; len(got) != len(want) ||
		got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("order = %x, want %x", got, want)
	}

	// Status, owner and purchase of each.
	all := list(&dentalv1.ListSubscriptionsRequest{})
	wantStatus := map[[16]byte]dentalv1.SubscriptionStatus{
		ended:    dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ENDED,
		active:   dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE,
		upcoming: dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_UPCOMING,
		canceled: dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED,
	}
	for _, item := range all.GetItems() {
		id := [16]byte(item.GetSubscription().GetId())
		if item.GetStatus() != wantStatus[id] {
			t.Errorf("status of %x = %v, want %v", id, item.GetStatus(), wantStatus[id])
		}
		if (item.GetPurchase() != nil) != (id == active) {
			t.Errorf("purchase of %x = %v", id, item.GetPurchase())
		}
		if item.GetIdentity().GetNumber() == "" || item.GetPhone() == "" || item.GetUser().GetUsername() == "" {
			t.Errorf("owner of %x = %v", id, item)
		}
	}

	var bobCode string
	if err := e.deps.DB.QueryRow(ctx, `SELECT identity FROM users WHERE id = $1`, bob.GetId()).Scan(&bobCode); err != nil {
		t.Fatal(err)
	}
	// Filters: status, then text in the owner's name, identification code or phone.
	for _, tc := range []struct {
		req  *dentalv1.ListSubscriptionsRequest
		want uint32
	}{
		{&dentalv1.ListSubscriptionsRequest{Status: dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE}, 1},
		{&dentalv1.ListSubscriptionsRequest{Status: dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_CANCELED}, 1},
		{&dentalv1.ListSubscriptionsRequest{Query: "  DR. ALICE "}, 3},
		{&dentalv1.ListSubscriptionsRequest{Query: bobCode}, 1},
		{&dentalv1.ListSubscriptionsRequest{Query: "0912"}, 4},
		{&dentalv1.ListSubscriptionsRequest{Query: "bob", Status: dentalv1.SubscriptionStatus_SUBSCRIPTION_STATUS_ACTIVE}, 0},
		{&dentalv1.ListSubscriptionsRequest{Query: "%"}, 0},
	} {
		if res := list(tc.req); res.GetTotalCount() != tc.want || len(res.GetItems()) != int(tc.want) {
			t.Errorf("%v: %d of %d, want %d", tc.req, len(res.GetItems()), res.GetTotalCount(), tc.want)
		}
	}
	_, err = admin.ListSubscriptions(ctx, &dentalv1.ListSubscriptionsRequest{PageSize: 101})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// Payments: pending ones oldest first, the others newest first; filtered and paged the same way.
	payments := func(req *dentalv1.ListPaymentsRequest) ([]string, uint32) {
		t.Helper()
		res, err := admin.ListPayments(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		var refs []string
		for _, r := range res.GetPayments() {
			refs = append(refs, r.GetPayment().GetTransfer().GetReference())
		}
		return refs, res.GetTotalCount()
	}
	if refs, total := payments(&dentalv1.ListPaymentsRequest{PageSize: 2}); total != 3 || len(refs) != 2 ||
		refs[0] != "REF-BOB-2" || refs[1] != "REF-BOB-1" {
		t.Errorf("first page = %v of %d", refs, total)
	}
	if refs, total := payments(&dentalv1.ListPaymentsRequest{Page: 2, PageSize: 2}); total != 3 || len(refs) != 1 || refs[0] != "REF-ALICE" {
		t.Errorf("second page = %v of %d", refs, total)
	}
	if refs, total := payments(&dentalv1.ListPaymentsRequest{Query: "ref-bob"}); total != 2 || len(refs) != 2 {
		t.Errorf("by reference = %v of %d", refs, total)
	}
	if refs, total := payments(&dentalv1.ListPaymentsRequest{
		Status: dentalv1.PaymentStatus_PAYMENT_STATUS_APPROVED, Query: "alice",
	}); total != 1 || refs[0] != "REF-ALICE" {
		t.Errorf("approved of alice = %v of %d", refs, total)
	}
	if refs, total := payments(&dentalv1.ListPaymentsRequest{Page: 9}); total != 3 || len(refs) != 0 {
		t.Errorf("past the end = %v of %d", refs, total)
	}
}

func TestAdminListsPatients(t *testing.T) {
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

	// Three patients from one clinic; Sara is also treated in a second one.
	aliceToken := e.clinicToken(1, "alice")
	bobToken := e.clinicToken(2, "bob")
	now := time.Now()
	sara := testPatient(personCode(7001), "Sara", now)
	sara.Phone, sara.Email = "09121112233", "Sara.Ahmadi@example.com"
	reza := testPatient(personCode(7002), "Reza", now)
	reza.LastName, reza.Phone = "Karimi", "09354445566"
	mina := testPatient(personCode(7003), "Mina", now)
	removed := testTreatment(2, sara, "Wrong entry", now)
	removed.DeletedAt = timestamppb.Now()
	if _, err := e.sync(aliceToken).Push(ctx, &dentalv1.PushRequest{
		Patients:   []*dentalv1.Patient{sara, reza, mina},
		Treatments: []*dentalv1.Treatment{testTreatment(1, sara, "Filling", now), removed},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.sync(bobToken).Push(ctx, &dentalv1.PushRequest{
		Treatments: []*dentalv1.Treatment{testTreatment(3, sara, "Cleaning", now)},
	}); err != nil {
		t.Fatal(err)
	}
	// Received one after another: Sara first, Mina last.
	for i, p := range []*dentalv1.Patient{sara, reza, mina} {
		if _, err := e.deps.DB.Exec(ctx, `UPDATE patients SET created_at = now() - make_interval(days => $2) WHERE id = $1`,
			p.GetId(), 3-i); err != nil {
			t.Fatal(err)
		}
	}

	list := func(req *dentalv1.ListPatientsRequest) ([]string, uint32) {
		t.Helper()
		res, err := admin.ListPatients(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range res.GetPatients() {
			names = append(names, p.GetFirstName())
		}
		return names, res.GetTotalCount()
	}

	// Newest first, a page at a time, with the total on every page.
	if names, total := list(&dentalv1.ListPatientsRequest{PageSize: 2}); total != 3 || len(names) != 2 ||
		names[0] != "Mina" || names[1] != "Reza" {
		t.Errorf("first page = %v of %d", names, total)
	}
	if names, total := list(&dentalv1.ListPatientsRequest{Page: 2, PageSize: 2}); total != 3 || len(names) != 1 || names[0] != "Sara" {
		t.Errorf("second page = %v of %d", names, total)
	}

	// Clinics and treatments (deleted ones left out) of each.
	res, err := admin.ListPatients(ctx, &dentalv1.ListPatientsRequest{Query: personCode(7001)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetPatients()) != 1 {
		t.Fatalf("by code = %v", res.GetPatients())
	}
	if got := res.GetPatients()[0]; got.GetClinicCount() != 2 || got.GetTreatmentCount() != 2 ||
		got.GetPhone() != "09121112233" || got.GetEmail() != "Sara.Ahmadi@example.com" || got.GetGender() != dentalv1.Gender_GENDER_FEMALE ||
		[16]byte(got.GetId()) != [16]byte(sara.GetId()) || got.GetCreatedAt() == nil || got.GetUpdatedAt() == nil {
		t.Errorf("sara = %v", got)
	}

	// Text in the full name, identification code, phone or email.
	for _, tc := range []struct {
		query string
		want  uint32
	}{
		{"  reza KARIMI ", 1},
		{"ahmadi", 2},
		{"0935", 1},
		{"SARA.AHMADI@", 1},
		{"09", 2},
		{personCode(7001)[:9], 3},
		{"nobody", 0},
		{"%", 0},
	} {
		if names, total := list(&dentalv1.ListPatientsRequest{Query: tc.query}); total != tc.want || len(names) != int(tc.want) {
			t.Errorf("%q: %v of %d, want %d", tc.query, names, total, tc.want)
		}
	}
	_, err = admin.ListPatients(ctx, &dentalv1.ListPatientsRequest{PageSize: 101})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
}

func TestAdminListsUsers(t *testing.T) {
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

	// alice owns a clinic, is subscribed and has logged in; bob has an email;
	// carol is disabled. Created one after another: alice first, carol last.
	inst := e.register(1, "alice")
	e.subscribe("alice", 1, 1, 30)
	if _, err := e.login(inst, "alice", "Passw0rd-alice"); err != nil {
		t.Fatal(err)
	}
	e.signUp("bob")
	e.signUp("carol")
	if _, err := e.deps.DB.Exec(ctx, `
		UPDATE users SET created_at = now() - make_interval(days => CASE username WHEN 'alice' THEN 3 WHEN 'bob' THEN 2 ELSE 1 END),
		                 email = CASE username WHEN 'bob' THEN 'Bob@Clinic.example' ELSE '' END,
		                 disabled_at = CASE username WHEN 'carol' THEN now() END`); err != nil {
		t.Fatal(err)
	}

	list := func(req *dentalv1.ListUsersRequest) ([]*dentalv1.UserListItem, uint32) {
		t.Helper()
		res, err := admin.ListUsers(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return res.GetUsers(), res.GetTotalCount()
	}
	names := func(users []*dentalv1.UserListItem) []string {
		var names []string
		for _, u := range users {
			names = append(names, u.GetUser().GetUsername())
		}
		return names
	}

	// Newest first, a page at a time, with the total on every page.
	if users, total := list(&dentalv1.ListUsersRequest{PageSize: 2}); total != 3 || len(users) != 2 ||
		names(users)[0] != "carol" || names(users)[1] != "bob" {
		t.Errorf("first page = %v of %d", names(users), total)
	}
	if users, total := list(&dentalv1.ListUsersRequest{Page: 2, PageSize: 2}); total != 3 || len(users) != 1 ||
		names(users)[0] != "alice" {
		t.Errorf("second page = %v of %d", names(users), total)
	}

	users, _ := list(&dentalv1.ListUsersRequest{})
	byName := map[string]*dentalv1.UserListItem{}
	for _, u := range users {
		byName[u.GetUser().GetUsername()] = u
	}
	alice, bob, carol := byName["alice"], byName["bob"], byName["carol"]
	if alice.GetOwnedClinicCount() != 1 || alice.GetMemberClinicCount() != 1 || alice.GetPlanName() != "test" ||
		alice.GetPaidUntil() == nil || time.Until(alice.GetPaidUntil().AsTime()) < 29*24*time.Hour ||
		alice.GetLastLoginAt() == nil || alice.GetIdentity().GetNumber() != personCode(1) ||
		alice.GetPhone() != "09121234567" || alice.GetEmail() != "" || alice.GetDisabled() ||
		len(alice.GetUser().GetId()) != 16 || alice.GetCreatedAt() == nil {
		t.Errorf("alice = %v", alice)
	}
	if bob.GetEmail() != "Bob@Clinic.example" || bob.GetPlanName() != "" || bob.GetPaidUntil() != nil ||
		bob.GetLastLoginAt() != nil || bob.GetOwnedClinicCount() != 0 || bob.GetMemberClinicCount() != 0 || bob.GetDisabled() {
		t.Errorf("bob = %v", bob)
	}
	if !carol.GetDisabled() {
		t.Errorf("carol = %v", carol)
	}

	// Text in the full name, username, identification code, phone or email.
	for _, tc := range []struct {
		query string
		want  uint32
	}{
		{"  DR. ALICE ", 1},
		{"dr. ", 3},
		{"carol", 1},
		{personCode(1), 1},
		{"0912", 3},
		{"bob@clinic", 1},
		{"nobody", 0},
		{"%", 0},
	} {
		if users, total := list(&dentalv1.ListUsersRequest{Query: tc.query}); total != tc.want || len(users) != int(tc.want) {
			t.Errorf("%q: %v of %d, want %d", tc.query, names(users), total, tc.want)
		}
	}
	_, err = admin.ListUsers(ctx, &dentalv1.ListUsersRequest{PageSize: 101})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)

	// Not without an administrator's session.
	_, err = e.admin(adminSession{}).ListUsers(ctx, &dentalv1.ListUsersRequest{})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}
