package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

// SyncService keeps a clinic's patients and treatments the same on all its
// installations (see proto/dental/v1/sync.proto). Request fields are checked by
// ValidateInterceptor; the handlers check what the rules there cannot.
type SyncService struct {
	d *Deps
}

var _ dentalv1connect.SyncServiceHandler = (*SyncService)(nil)

func NewSyncService(d *Deps) *SyncService {
	return &SyncService{d: d}
}

// Records per kind in one PullResponse. A variable so tests can page with small numbers.
var pullPageSize = 500

// syncWriteLock is the advisory lock every synced write holds until it commits.
// Revisions are taken inside it, so they become visible in the order they were
// taken: a Pull that saw revision N has also seen every revision below N, and an
// installation can safely continue from N next time. Patients are shared by
// clinics, so the lock is global rather than per clinic; pushes are small and short.
const syncWriteLock = 0x64656e74616c // "dental"

// Reasons for RejectedRecord.reason.
const (
	rejectInvalidPatientID = "invalid_patient_id"
	rejectUnknownPatient   = "unknown_patient"
	rejectUnknownUser      = "unknown_user"
	rejectOtherClinic      = "other_clinic"
)

// patientID derives a patient's id from their identification code (see Patient.id
// in patient.proto). Migration 00015_identity_code.sql computes the same in SQL.
func patientID(number string) []byte {
	sum := sha256.Sum256([]byte("dental/patient/v2\x00" + number))
	id := sum[:16]
	id[6] = id[6]&0x0f | 0x80 // version 8
	id[8] = id[8]&0x3f | 0x80 // RFC 4122 variant
	return id
}

var genderNames = map[dentalv1.Gender]string{
	dentalv1.Gender_GENDER_UNSPECIFIED: "",
	dentalv1.Gender_GENDER_MALE:        "male",
	dentalv1.Gender_GENDER_FEMALE:      "female",
}

func genderFromName(name string) dentalv1.Gender {
	for g, n := range genderNames {
		if n == name {
			return g
		}
	}
	return dentalv1.Gender_GENDER_UNSPECIFIED
}

// clientTime is a time set on a client's clock, never later than the server's:
// a clock running ahead must not let its changes win forever.
func clientTime(t *timestamppb.Timestamp, now time.Time) time.Time {
	if at := t.AsTime(); at.Before(now) {
		return at
	}
	return now
}

func optionalUUID(b []byte) pgtype.UUID {
	id, _ := toUUID(b)
	return id
}

func (s *SyncService) Push(ctx context.Context, req *dentalv1.PushRequest) (*dentalv1.PushResponse, error) {
	session := sessionFrom(ctx)
	now := s.d.Now()

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(syncWriteLock)); err != nil {
		return nil, internal(err)
	}

	res := &dentalv1.PushResponse{}
	reject := func(id []byte, reason string) {
		res.Rejected = append(res.Rejected, &dentalv1.RejectedRecord{Id: id, Reason: reason})
	}
	for _, p := range req.GetPatients() {
		reason, err := pushPatient(ctx, tx, session, p, now)
		switch {
		case err != nil:
			return nil, internal(err)
		case reason != "":
			reject(p.GetId(), reason)
		default:
			res.Accepted++
		}
	}
	for _, t := range req.GetTreatments() {
		reason, err := pushTreatment(ctx, tx, session, t, now)
		switch {
		case err != nil:
			return nil, internal(err)
		case reason != "":
			reject(t.GetId(), reason)
		default:
			res.Accepted++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// pushPatient stores a patient's details if they are newer than the server's, and
// links the patient to the session's clinic. Returns a reject reason, or "".
func pushPatient(ctx context.Context, tx pgx.Tx, session Session, p *dentalv1.Patient, now time.Time) (string, error) {
	number := p.GetIdentity().GetNumber()
	if string(patientID(number)) != string(p.GetId()) {
		return rejectInvalidPatientID, nil
	}
	id := optionalUUID(p.GetId())

	// The identification code never changes: it is what the id is made from.
	_, err := tx.Exec(ctx, `
		INSERT INTO patients (id, identity, first_name, last_name, birth_date,
		                      gender, phone, address, medical_notes, updated_at, updated_by, updated_in)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET
			first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name,
			birth_date = EXCLUDED.birth_date, gender = EXCLUDED.gender, phone = EXCLUDED.phone,
			address = EXCLUDED.address, medical_notes = EXCLUDED.medical_notes,
			updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by, updated_in = EXCLUDED.updated_in,
			revision = nextval('sync_revision_seq')
		WHERE patients.updated_at < EXCLUDED.updated_at`,
		id, number, p.GetFirstName(), p.GetLastName(),
		p.GetBirthDate(), genderNames[p.GetGender()], p.GetPhone(), p.GetAddress(), p.GetMedicalNotes(),
		clientTime(p.GetUpdatedAt(), now), session.UserID, session.ClinicID,
	)
	if err != nil {
		return "", err
	}
	return "", linkPatient(ctx, tx, session.ClinicID, id)
}

// linkPatient makes sure the clinic receives the patient's record from now on.
func linkPatient(ctx context.Context, tx pgx.Tx, clinicID, patientID pgtype.UUID) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO clinic_patients (clinic_id, patient_id) VALUES ($1, $2)
		ON CONFLICT (clinic_id, patient_id) DO NOTHING`, clinicID, patientID)
	return err
}

// pushTreatment stores a treatment of the session's clinic if it is newer than the
// server's copy. Returns a reject reason, or "".
func pushTreatment(ctx context.Context, tx pgx.Tx, session Session, t *dentalv1.Treatment, now time.Time) (string, error) {
	id := optionalUUID(t.GetId())
	patient := optionalUUID(t.GetPatientId())
	performedBy := optionalUUID(t.GetPerformedBy())

	var patientKnown, userKnown bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM patients WHERE id = $1),
		       $2::uuid IS NULL OR EXISTS (SELECT 1 FROM users WHERE id = $2)`,
		patient, performedBy,
	).Scan(&patientKnown, &userKnown)
	switch {
	case err != nil:
		return "", err
	case !patientKnown:
		return rejectUnknownPatient, nil
	case !userKnown:
		return rejectUnknownUser, nil
	}

	var deletedAt pgtype.Timestamptz
	if t.GetDeletedAt() != nil {
		deletedAt = pgtype.Timestamptz{Time: clientTime(t.GetDeletedAt(), now), Valid: true}
	}
	var stored bool
	err = tx.QueryRow(ctx, `
		INSERT INTO treatments (id, patient_id, clinic_id, performed_by, performed_at, tooth, procedure,
		                        notes, updated_at, deleted_at, updated_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			patient_id = EXCLUDED.patient_id, performed_by = EXCLUDED.performed_by,
			performed_at = EXCLUDED.performed_at, tooth = EXCLUDED.tooth, procedure = EXCLUDED.procedure,
			notes = EXCLUDED.notes, updated_at = EXCLUDED.updated_at, deleted_at = EXCLUDED.deleted_at,
			updated_by = EXCLUDED.updated_by, revision = nextval('sync_revision_seq')
		WHERE treatments.clinic_id = EXCLUDED.clinic_id AND treatments.updated_at < EXCLUDED.updated_at
		RETURNING true`,
		id, patient, session.ClinicID, performedBy, t.GetPerformedAt().AsTime(), t.GetTooth(), t.GetProcedure(),
		t.GetNotes(), clientTime(t.GetUpdatedAt(), now), deletedAt, session.UserID,
	).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		// Not stored: the server's copy is newer, or it is another clinic's treatment.
		var owner pgtype.UUID
		if err := tx.QueryRow(ctx, `SELECT clinic_id FROM treatments WHERE id = $1`, id).Scan(&owner); err != nil {
			return "", err
		}
		if owner != session.ClinicID {
			return rejectOtherClinic, nil
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "", linkPatient(ctx, tx, session.ClinicID, patient)
}

func (s *SyncService) Pull(ctx context.Context, req *dentalv1.PullRequest) (*dentalv1.PullResponse, error) {
	session := sessionFrom(ctx)
	since := req.GetSinceRevision()

	// A patient is new to the clinic when its details changed, or when it was just
	// linked to the clinic (its details may be older than what the clinic pulled).
	patients, patientRevisions, err := queryPatients(ctx, s.d.DB, `
		SELECT `+patientColumns+`, GREATEST(p.revision, cp.revision) AS changed
		FROM clinic_patients cp JOIN patients p ON p.id = cp.patient_id
		WHERE cp.clinic_id = $1 AND GREATEST(p.revision, cp.revision) > $2
		ORDER BY changed LIMIT $3`,
		session.ClinicID, since, pullPageSize)
	if err != nil {
		return nil, internal(err)
	}
	treatments, treatmentRevisions, err := queryTreatments(ctx, s.d.DB, `
		SELECT `+treatmentColumns+`, t.revision
		FROM treatments t
		WHERE t.clinic_id = $1 AND t.revision > $2
		ORDER BY t.revision LIMIT $3`,
		session.ClinicID, since, pullPageSize)
	if err != nil {
		return nil, internal(err)
	}

	// A full page may have more after it. Only what is below the end of every full
	// page is complete, so the response stops there and the client asks again.
	res := &dentalv1.PullResponse{Revision: since}
	upTo := int64(math.MaxInt64)
	for _, revisions := range [][]int64{patientRevisions, treatmentRevisions} {
		if len(revisions) == pullPageSize {
			upTo = min(upTo, revisions[len(revisions)-1])
			res.HasMore = true
		}
	}
	for i, p := range patients {
		if patientRevisions[i] <= upTo {
			res.Patients = append(res.Patients, p)
			res.Revision = max(res.Revision, patientRevisions[i])
		}
	}
	for i, t := range treatments {
		if treatmentRevisions[i] <= upTo {
			res.Treatments = append(res.Treatments, t)
			res.Revision = max(res.Revision, treatmentRevisions[i])
		}
	}
	return res, nil
}

const patientColumns = `p.id, p.identity, p.first_name, p.last_name, p.birth_date,
	p.gender, p.phone, p.address, p.medical_notes, p.updated_at`

// queryPatients runs a query selecting patientColumns plus one bigint (a revision).
func queryPatients(ctx context.Context, q querier, sql string, args ...any) ([]*dentalv1.Patient, []int64, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	var patients []*dentalv1.Patient
	var revisions []int64
	for rows.Next() {
		var revision int64
		p, err := scanPatient(rows, &revision)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		patients = append(patients, p)
		revisions = append(revisions, revision)
	}
	return patients, revisions, rows.Err()
}

// scanPatient reads patientColumns, then `extra`.
func scanPatient(row pgx.Row, extra ...any) (*dentalv1.Patient, error) {
	var id pgtype.UUID
	var gender string
	var updatedAt time.Time
	p := &dentalv1.Patient{Identity: &dentalv1.PersonIdentity{}}
	dest := append([]any{&id, &p.Identity.Number, &p.FirstName, &p.LastName, &p.BirthDate,
		&gender, &p.Phone, &p.Address, &p.MedicalNotes, &updatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	p.Id = uuidBytes(id)
	p.Gender = genderFromName(gender)
	p.UpdatedAt = timestamppb.New(updatedAt)
	return p, nil
}

const treatmentColumns = `t.id, t.patient_id, t.performed_by, t.performed_at, t.tooth, t.procedure,
	t.notes, t.updated_at, t.deleted_at`

// queryTreatments runs a query selecting treatmentColumns plus one bigint (a revision).
func queryTreatments(ctx context.Context, q querier, sql string, args ...any) ([]*dentalv1.Treatment, []int64, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, err
	}
	var treatments []*dentalv1.Treatment
	var revisions []int64
	for rows.Next() {
		var revision int64
		t, err := scanTreatment(rows, &revision)
		if err != nil {
			rows.Close()
			return nil, nil, err
		}
		treatments = append(treatments, t)
		revisions = append(revisions, revision)
	}
	return treatments, revisions, rows.Err()
}

// scanTreatment reads treatmentColumns, then `extra`.
func scanTreatment(row pgx.Row, extra ...any) (*dentalv1.Treatment, error) {
	var id, patient, performedBy pgtype.UUID
	var performedAt, updatedAt time.Time
	var deletedAt pgtype.Timestamptz
	t := &dentalv1.Treatment{}
	dest := append([]any{&id, &patient, &performedBy, &performedAt, &t.Tooth, &t.Procedure,
		&t.Notes, &updatedAt, &deletedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	t.Id = uuidBytes(id)
	t.PatientId = uuidBytes(patient)
	if performedBy.Valid {
		t.PerformedBy = uuidBytes(performedBy)
	}
	t.PerformedAt = timestamppb.New(performedAt)
	t.UpdatedAt = timestamppb.New(updatedAt)
	if deletedAt.Valid {
		t.DeletedAt = timestamppb.New(deletedAt.Time)
	}
	return t, nil
}
