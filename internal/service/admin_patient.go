package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
)

// Looking up patients synced from the clinics. Only who they are and how to reach
// them: addresses, medical notes and treatments are never listed here.

func (s *AdminService) ListPatients(ctx context.Context, req *dentalv1.ListPatientsRequest) (*dentalv1.ListPatientsResponse, error) {
	query := searchText(req.GetQuery())
	limit, offset := pageBounds(req.GetPage(), req.GetPageSize())

	from := `
		FROM patients p
		WHERE $1 = ''
		   OR strpos(lower(p.first_name || ' ' || p.last_name), $1) > 0
		   OR strpos(lower(p.identity), $1) > 0 OR strpos(p.phone, $1) > 0 OR strpos(lower(p.email), $1) > 0`

	res := &dentalv1.ListPatientsResponse{}
	var total int64
	if err := s.d.DB.QueryRow(ctx, `SELECT count(*)`+from, query).Scan(&total); err != nil {
		return nil, internal(err)
	}
	res.TotalCount = uint32(total)

	rows, err := s.d.DB.Query(ctx, `
		SELECT p.id, p.identity, p.first_name, p.last_name, p.birth_date, p.gender, p.phone, p.email,
		       (SELECT count(*) FROM clinic_patients cp WHERE cp.patient_id = p.id),
		       (SELECT count(*) FROM treatments t WHERE t.patient_id = p.id AND t.deleted_at IS NULL),
		       p.created_at, p.updated_at`+from+`
		ORDER BY p.created_at DESC, p.id DESC
		LIMIT $2 OFFSET $3`, query, limit, offset)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id pgtype.UUID
		var gender string
		var clinics, treatments int64
		var createdAt, updatedAt time.Time
		item := &dentalv1.PatientListItem{Identity: &dentalv1.PersonIdentity{}}
		if err := rows.Scan(&id, &item.Identity.Number, &item.FirstName, &item.LastName, &item.BirthDate,
			&gender, &item.Phone, &item.Email, &clinics, &treatments, &createdAt, &updatedAt); err != nil {
			return nil, internal(err)
		}
		item.Id = uuidBytes(id)
		item.Gender = genderFromName(gender)
		item.ClinicCount, item.TreatmentCount = uint32(clinics), uint32(treatments)
		item.CreatedAt, item.UpdatedAt = timestamppb.New(createdAt), timestamppb.New(updatedAt)
		res.Patients = append(res.Patients, item)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}
