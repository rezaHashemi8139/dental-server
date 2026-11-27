package service

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

// PatientService shows a patient's history across clinics (see proto/dental/v1/patient.proto).
type PatientService struct {
	d *Deps
}

var _ dentalv1connect.PatientServiceHandler = (*PatientService)(nil)

func NewPatientService(d *Deps) *PatientService {
	return &PatientService{d: d}
}

// GetPatientHistory is open to every member of a clinic for now; every view is
// logged in the same transaction, so no history is shown without its log entry.
func (s *PatientService) GetPatientHistory(ctx context.Context, req *dentalv1.GetPatientHistoryRequest) (*dentalv1.GetPatientHistoryResponse, error) {
	session := sessionFrom(ctx)
	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	patient, err := scanPatient(tx.QueryRow(ctx,
		`SELECT `+patientColumns+` FROM patients p WHERE p.identity = $1`, req.GetIdentity().GetNumber()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonPatientNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	patientID := optionalUUID(patient.GetId())

	if _, err := tx.Exec(ctx, `
		INSERT INTO patient_history_views (patient_id, user_id, clinic_id, installation_id)
		VALUES ($1, $2, $3, $4)`,
		patientID, session.UserID, session.ClinicID, session.InstallationID,
	); err != nil {
		return nil, internal(err)
	}

	rows, err := tx.Query(ctx, `
		SELECT `+treatmentColumns+`, c.name, c.city, coalesce(u.full_name, '')
		FROM treatments t
		JOIN clinics c ON c.id = t.clinic_id
		LEFT JOIN users u ON u.id = t.performed_by
		WHERE t.patient_id = $1 AND t.deleted_at IS NULL
		ORDER BY t.performed_at DESC, t.id`, patientID)
	if err != nil {
		return nil, internal(err)
	}
	res := &dentalv1.GetPatientHistoryResponse{Patient: patient}
	for rows.Next() {
		entry := &dentalv1.TreatmentHistoryEntry{}
		entry.Treatment, err = scanTreatment(rows, &entry.ClinicName, &entry.ClinicCity, &entry.PerformedByName)
		if err != nil {
			rows.Close()
			return nil, internal(err)
		}
		res.Treatments = append(res.Treatments, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return res, nil
}
