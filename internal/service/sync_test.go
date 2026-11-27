package service

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/releases"
)

// The desktop app derives the same id (src-tauri/src/patients.rs, patient_id_matches_the_server).
func TestPatientID(t *testing.T) {
	got := hex.EncodeToString(patientID("0012345679"))
	if got != "3cc05a27428a8621a9efab0ad5d2b80a" {
		t.Fatalf("patientID = %s", got)
	}
}

// clinicToken registers a clinic for a new owner, gives them a subscription and logs them in.
func (e *testEnv) clinicToken(machine byte, owner string) string {
	e.t.Helper()
	inst := e.register(machine, owner)
	e.subscribe(owner, 1, 2, 30)
	login, err := e.login(inst, owner, "Passw0rd-"+owner)
	if err != nil {
		e.t.Fatal(err)
	}
	return login.res.GetSessionToken()
}

func (e *testEnv) sync(token string) dentalv1connect.SyncServiceClient {
	return dentalv1connect.NewSyncServiceClient(e.server.Client(), e.server.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
}

func (e *testEnv) patients(token string) dentalv1connect.PatientServiceClient {
	return dentalv1connect.NewPatientServiceClient(e.server.Client(), e.server.URL, connect.WithGRPC(),
		connect.WithInterceptors(bearer(token)))
}

func testPatient(code, firstName string, updatedAt time.Time) *dentalv1.Patient {
	return &dentalv1.Patient{
		Id:        patientID(code),
		Identity:  personIdentity(code),
		FirstName: firstName,
		LastName:  "Ahmadi",
		Gender:    dentalv1.Gender_GENDER_FEMALE,
		UpdatedAt: timestamppb.New(updatedAt),
	}
}

func testTreatment(id byte, patient *dentalv1.Patient, procedure string, updatedAt time.Time) *dentalv1.Treatment {
	return &dentalv1.Treatment{
		Id:          []byte{id, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		PatientId:   patient.GetId(),
		PerformedAt: timestamppb.New(updatedAt.Add(-time.Hour)),
		Tooth:       "36",
		Procedure:   procedure,
		UpdatedAt:   timestamppb.New(updatedAt),
	}
}

func pullAll(t *testing.T, client dentalv1connect.SyncServiceClient, since int64) *dentalv1.PullResponse {
	t.Helper()
	all := &dentalv1.PullResponse{Revision: since}
	for range 100 {
		res, err := client.Pull(context.Background(), &dentalv1.PullRequest{SinceRevision: all.GetRevision()})
		if err != nil {
			t.Fatal(err)
		}
		all.Patients = append(all.Patients, res.GetPatients()...)
		all.Treatments = append(all.Treatments, res.GetTreatments()...)
		all.Revision = res.GetRevision()
		if !res.GetHasMore() {
			return all
		}
	}
	t.Fatal("pull never ended")
	return nil
}

func TestSyncBetweenClinics(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	alice := e.sync(e.clinicToken(1, "alice"))
	bob := e.sync(e.clinicToken(2, "bob"))
	now := time.Now()

	// Clinic A records a patient and a treatment.
	sara := testPatient(personCode(5000), "Sara", now.Add(-time.Hour))
	filling := testTreatment(1, sara, "Filling", now.Add(-time.Hour))
	res, err := alice.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{sara}, Treatments: []*dentalv1.Treatment{filling}})
	if err != nil || res.GetAccepted() != 2 || len(res.GetRejected()) != 0 {
		t.Fatalf("push: %v %v", res, err)
	}

	// Clinic B knows the same person, with an older change: taken, but A's details stay.
	older := testPatient(personCode(5000), "S.", now.Add(-2*time.Hour))
	if _, err := bob.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{older}}); err != nil {
		t.Fatal(err)
	}
	b := pullAll(t, bob, 0)
	if len(b.GetPatients()) != 1 || b.GetPatients()[0].GetFirstName() != "Sara" {
		t.Fatalf("B pulled %v", b.GetPatients())
	}
	if len(b.GetTreatments()) != 0 {
		t.Fatal("B received A's treatment: treatments belong to their clinic")
	}
	if got := b.GetPatients()[0].GetIdentity(); got.GetNumber() != personCode(5000) {
		t.Fatalf("identity = %v", got)
	}

	// A newer change from B wins, and A receives it from where it stopped.
	a := pullAll(t, alice, 0)
	if len(a.GetPatients()) != 1 || len(a.GetTreatments()) != 1 {
		t.Fatalf("A pulled %d patients, %d treatments", len(a.GetPatients()), len(a.GetTreatments()))
	}
	newer := testPatient(personCode(5000), "Sarah", now.Add(-time.Minute))
	newer.Phone, newer.Email = "09121234567", "sarah@example.com"
	if _, err := bob.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{newer}}); err != nil {
		t.Fatal(err)
	}
	next := pullAll(t, alice, a.GetRevision())
	if len(next.GetPatients()) != 1 || next.GetPatients()[0].GetFirstName() != "Sarah" ||
		next.GetPatients()[0].GetEmail() != "sarah@example.com" || len(next.GetTreatments()) != 0 {
		t.Fatalf("A pulled after B's change: %v", next)
	}
	if again := pullAll(t, alice, next.GetRevision()); len(again.GetPatients())+len(again.GetTreatments()) != 0 {
		t.Fatal("nothing new, but the pull returned records")
	}

	// A change dated in the future counts as made now: it cannot win forever.
	future := testPatient(personCode(5000), "Future", now.Add(24*time.Hour))
	if _, err := alice.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{future}}); err != nil {
		t.Fatal(err)
	}
	latest := testPatient(personCode(5000), "Later", time.Now().Add(time.Second))
	if _, err := bob.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{latest}}); err != nil {
		t.Fatal(err)
	}
	if got := pullAll(t, alice, 0).GetPatients()[0].GetFirstName(); got != "Later" {
		t.Fatalf("first name = %q, want the later change", got)
	}

	// A deletion travels as a tombstone to the clinic's installations.
	deleted := testTreatment(1, sara, "Filling", time.Now())
	deleted.DeletedAt = timestamppb.Now()
	if _, err := alice.Push(ctx, &dentalv1.PushRequest{Treatments: []*dentalv1.Treatment{deleted}}); err != nil {
		t.Fatal(err)
	}
	tombstones := pullAll(t, alice, next.GetRevision()).GetTreatments()
	if len(tombstones) != 1 || tombstones[0].GetDeletedAt() == nil {
		t.Fatalf("tombstone = %v", tombstones)
	}
}

func TestSyncRejections(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	alice := e.sync(e.clinicToken(1, "alice"))
	bob := e.sync(e.clinicToken(2, "bob"))
	now := time.Now()

	sara := testPatient(personCode(5000), "Sara", now)
	alicesTreatment := testTreatment(1, sara, "Filling", now)
	if _, err := alice.Push(ctx, &dentalv1.PushRequest{Patients: []*dentalv1.Patient{sara}, Treatments: []*dentalv1.Treatment{alicesTreatment}}); err != nil {
		t.Fatal(err)
	}

	wrongID := testPatient(personCode(5001), "Wrong", now)
	wrongID.Id = sara.GetId()
	stranger := testPatient(personCode(5002), "Stranger", now)
	unknownPatient := testTreatment(2, stranger, "Cleaning", now)
	unknownUser := testTreatment(3, sara, "Cleaning", now)
	unknownUser.PerformedBy = make([]byte, 16)
	stolen := testTreatment(1, sara, "Changed by another clinic", now.Add(time.Second))

	res, err := bob.Push(ctx, &dentalv1.PushRequest{
		Patients:   []*dentalv1.Patient{wrongID},
		Treatments: []*dentalv1.Treatment{unknownPatient, unknownUser, stolen},
	})
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, r := range res.GetRejected() {
		reasons[hex.EncodeToString(r.GetId())] = r.GetReason()
	}
	want := map[string]string{
		hex.EncodeToString(wrongID.GetId()):        rejectInvalidPatientID,
		hex.EncodeToString(unknownPatient.GetId()): rejectUnknownPatient,
		hex.EncodeToString(unknownUser.GetId()):    rejectUnknownUser,
		hex.EncodeToString(stolen.GetId()):         rejectOtherClinic,
	}
	if len(reasons) != len(want) || res.GetAccepted() != 0 {
		t.Fatalf("rejected = %v, accepted %d", reasons, res.GetAccepted())
	}
	for id, reason := range want {
		if reasons[id] != reason {
			t.Errorf("%s: reason %q, want %q", id, reasons[id], reason)
		}
	}
	if got := pullAll(t, alice, 0).GetTreatments()[0].GetProcedure(); got != "Filling" {
		t.Fatalf("another clinic changed A's treatment: %q", got)
	}

	// Invalid input fails the whole request.
	tooMany := make([]*dentalv1.Patient, 501)
	for i := range tooMany {
		tooMany[i] = sara
	}
	_, err = bob.Push(ctx, &dentalv1.PushRequest{Patients: tooMany})
	wantCode(t, err, connect.CodeInvalidArgument, reasonInvalidArgument)
}

func TestPullPages(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	alice := e.sync(e.clinicToken(1, "alice"))
	defer func(size int) { pullPageSize = size }(pullPageSize)
	pullPageSize = 2

	now := time.Now()
	var patients []*dentalv1.Patient
	var treatments []*dentalv1.Treatment
	for i := range 5 {
		p := testPatient(personCode(6000+i), "P", now)
		patients = append(patients, p)
		treatments = append(treatments, testTreatment(byte(10+i), p, "Check-up", now))
	}
	if _, err := alice.Push(ctx, &dentalv1.PushRequest{Patients: patients, Treatments: treatments}); err != nil {
		t.Fatal(err)
	}

	first, err := alice.Pull(ctx, &dentalv1.PullRequest{})
	if err != nil || !first.GetHasMore() {
		t.Fatalf("first page: %v %v", first, err)
	}
	all := pullAll(t, alice, 0)
	if len(all.GetPatients()) != 5 || len(all.GetTreatments()) != 5 {
		t.Fatalf("paged pull returned %d patients, %d treatments", len(all.GetPatients()), len(all.GetTreatments()))
	}
}

func TestPatientHistory(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	aliceToken := e.clinicToken(1, "alice")
	bobToken := e.clinicToken(2, "bob")
	now := time.Now()

	sara := testPatient(personCode(5000), "Sara", now)
	done := testTreatment(1, sara, "Root canal", now)
	var aliceID [16]byte
	if err := e.deps.DB.QueryRow(ctx, `SELECT id FROM users WHERE username = 'alice'`).Scan(&aliceID); err != nil {
		t.Fatal(err)
	}
	done.PerformedBy = aliceID[:]
	removed := testTreatment(2, sara, "Wrong entry", now)
	removed.DeletedAt = timestamppb.Now()
	if _, err := e.sync(aliceToken).Push(ctx, &dentalv1.PushRequest{
		Patients: []*dentalv1.Patient{sara}, Treatments: []*dentalv1.Treatment{done, removed},
	}); err != nil {
		t.Fatal(err)
	}

	// Another clinic looks the patient up by identification code.
	history, err := e.patients(bobToken).GetPatientHistory(ctx, &dentalv1.GetPatientHistoryRequest{Identity: personIdentity(personCode(5000))})
	if err != nil {
		t.Fatal(err)
	}
	if history.GetPatient().GetFirstName() != "Sara" || len(history.GetTreatments()) != 1 {
		t.Fatalf("history = %v", history)
	}
	entry := history.GetTreatments()[0]
	if entry.GetTreatment().GetProcedure() != "Root canal" || entry.GetClinicName() != "Clinic alice" || entry.GetPerformedByName() != "Dr. alice" {
		t.Fatalf("entry = %v", entry)
	}

	// The view is logged: who, from which clinic.
	var views int
	if err := e.deps.DB.QueryRow(ctx, `
		SELECT count(*) FROM patient_history_views v JOIN users u ON u.id = v.user_id
		WHERE u.username = 'bob' AND v.clinic_id = (SELECT clinic_id FROM clinic_members WHERE user_id = u.id)`,
	).Scan(&views); err != nil {
		t.Fatal(err)
	}
	if views != 1 {
		t.Fatalf("%d logged views, want 1", views)
	}

	_, err = e.patients(bobToken).GetPatientHistory(ctx, &dentalv1.GetPatientHistoryRequest{Identity: personIdentity(personCode(5999))})
	wantCode(t, err, connect.CodeNotFound, reasonPatientNotFound)
	_, err = e.patients("not-a-token").GetPatientHistory(ctx, &dentalv1.GetPatientHistoryRequest{Identity: personIdentity(personCode(5000))})
	wantCode(t, err, connect.CodeUnauthenticated, reasonUnauthenticated)
}

// Apps older than the release's minimum version are refused everything but the
// health check (which tells them to update) and languages.
func TestAppVersionInterceptor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.exe"), []byte("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	release := `{"version": "0.3.0", "minimumVersion": "0.2.0",
		"platforms": {"windows-x86_64": {"file": "setup.exe", "signature": "c2ln"}}}`
	if err := os.WriteFile(filepath.Join(dir, releases.FileName), []byte(release), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := releases.Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	errReached := errors.New("reached")
	reached := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if req.Spec().Procedure == dentalv1connect.HealthServiceCheckProcedure {
				return next(ctx, req)
			}
			return nil, connect.NewError(connect.CodeUnimplemented, errReached)
		}
	})
	interceptors := connect.WithInterceptors(AppVersionInterceptor(store), reached)
	mux := http.NewServeMux()
	mux.Handle(dentalv1connect.NewHealthServiceHandler(NewHealthService(nil, "test", func() error { return errReached }, store), interceptors))
	mux.Handle(dentalv1connect.NewSyncServiceHandler(NewSyncService(nil), interceptors))
	server := httptest.NewServer(mux)
	defer server.Close()

	withVersion := func(version string) connect.ClientOption {
		return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				if version != "" {
					req.Header().Set(AppVersionHeader, version)
				}
				return next(ctx, req)
			}
		}))
	}
	pull := func(version string) error {
		client := dentalv1connect.NewSyncServiceClient(server.Client(), server.URL, withVersion(version))
		_, err := client.Pull(context.Background(), &dentalv1.PullRequest{})
		return err
	}

	wantCode(t, pull("0.1.9"), connect.CodeFailedPrecondition, reasonUpdateRequired)
	wantCode(t, pull(""), connect.CodeFailedPrecondition, reasonUpdateRequired)
	if err := pull("0.2.0"); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("minimum version refused: %v", err)
	}

	health := dentalv1connect.NewHealthServiceClient(server.Client(), server.URL, withVersion("0.1.0"))
	res, err := health.Check(context.Background(), &dentalv1.CheckRequest{})
	if err != nil || res.GetMinimumAppVersion() != "0.2.0" {
		t.Fatalf("health check of an outdated app: %v %v", res, err)
	}
}
