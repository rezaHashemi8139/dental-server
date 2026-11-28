package service

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
)

// One subscription in full. ListSubscriptions (admin.go) lists them.

func (s *AdminService) GetSubscription(ctx context.Context, req *dentalv1.GetSubscriptionRequest) (*dentalv1.GetSubscriptionResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	items, err := s.subscriptionItems(ctx, ` WHERE s.id = $2`, s.d.Now(), id)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fail(connect.CodeNotFound, reasonSubscriptionNotFound)
	}
	res := &dentalv1.GetSubscriptionResponse{Item: items[0]}
	sub := res.Item.GetSubscription()
	ownerID, _ := toUUID(sub.GetUserId())
	if err := s.d.DB.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, ownerID).Scan(&res.Email); err != nil {
		return nil, internal(err)
	}
	if res.Clinics, err = clinicDetailsOf(ctx, s.d.DB, ownerID, sub); err != nil {
		return nil, err
	}
	return res, nil
}

// clinicDetailsOf returns the owner's clinics, oldest first, with all their installations,
// and which of them are beyond the limits of the subscription sub (see checkClinicAccess).
func clinicDetailsOf(ctx context.Context, q querier, ownerID pgtype.UUID, sub *dentalv1.Subscription) ([]*dentalv1.ClinicDetail, error) {
	rows, err := q.Query(ctx, `
		SELECT c.id, c.name, c.license_number, c.phone, coalesce(pr.name, ''), c.city, c.address, c.postal_code,
		       c.latitude, c.longitude, c.created_at, c.updated_at,
		       (SELECT count(*) FROM clinic_members m WHERE m.clinic_id = c.id),
		       (SELECT count(*) FROM clinic_patients cp WHERE cp.clinic_id = c.id)
		FROM clinics c
		LEFT JOIN provinces pr ON pr.id = c.province_id
		WHERE c.owner_id = $1
		ORDER BY c.created_at, c.id`, ownerID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	var clinics []*dentalv1.ClinicDetail
	byClinic := map[pgtype.UUID]*dentalv1.ClinicDetail{}
	var ids []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		var latitude, longitude pgtype.Float8
		var createdAt, updatedAt time.Time
		var members, patients int64
		c := &dentalv1.ClinicDetail{}
		if err := rows.Scan(&id, &c.Name, &c.LicenseNumber, &c.Phone, &c.Province, &c.City, &c.Address, &c.PostalCode,
			&latitude, &longitude, &createdAt, &updatedAt, &members, &patients); err != nil {
			return nil, internal(err)
		}
		c.Id = uuidBytes(id)
		if latitude.Valid && longitude.Valid {
			c.Location = &dentalv1.GeoLocation{Latitude: latitude.Float64, Longitude: longitude.Float64}
		}
		c.CreatedAt, c.UpdatedAt = timestamppb.New(createdAt), timestamppb.New(updatedAt)
		c.MemberCount, c.PatientCount = uint32(members), uint32(patients)
		c.OverClinicLimit = int64(len(clinics)+1) > int64(sub.GetMaxClinics())
		byClinic[id] = c
		ids = append(ids, id)
		clinics = append(clinics, c)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	if len(ids) == 0 {
		return clinics, nil
	}

	// The rank counts the clinic's installations that are not revoked, oldest first, as
	// checkClinicAccess does. The last login is the newest session on the installation.
	installations, err := q.Query(ctx, `
		WITH last_login AS (
			SELECT DISTINCT ON (se.installation_id) se.installation_id, se.created_at, u.id, u.username, u.full_name
			FROM sessions se
			JOIN installations i ON i.id = se.installation_id
			JOIN users u ON u.id = se.user_id
			WHERE i.clinic_id = ANY($1)
			ORDER BY se.installation_id, se.created_at DESC)
		SELECT i.clinic_id, i.id, i.machine_id_hash, i.hostname, i.os_name, i.os_version, i.cpu_model, i.cpu_cores,
		       i.memory_bytes, i.mac_addresses, i.app_version, extract(epoch FROM i.offline_access)::bigint,
		       i.created_at, i.last_seen_at, i.revoked_at,
		       CASE WHEN i.revoked_at IS NULL THEN
		           row_number() OVER (PARTITION BY i.clinic_id, i.revoked_at IS NULL ORDER BY i.created_at, i.id) END,
		       l.created_at, l.id, l.username, l.full_name
		FROM installations i
		LEFT JOIN last_login l ON l.installation_id = i.id
		WHERE i.clinic_id = ANY($1)
		ORDER BY i.revoked_at IS NOT NULL, i.created_at DESC, i.id DESC`, ids)
	if err != nil {
		return nil, internal(err)
	}
	defer installations.Close()
	for installations.Next() {
		var clinicID, id, loginUserID pgtype.UUID
		var cpuCores int32
		var memoryBytes, offlineSeconds int64
		var createdAt, lastSeenAt time.Time
		var revokedAt, loginAt pgtype.Timestamptz
		var rank pgtype.Int8
		var loginUsername, loginFullName pgtype.Text
		inst := &dentalv1.InstallationDetail{}
		if err := installations.Scan(&clinicID, &id, &inst.MachineIdHash, &inst.Hostname, &inst.OsName, &inst.OsVersion,
			&inst.CpuModel, &cpuCores, &memoryBytes, &inst.MacAddresses, &inst.AppVersion, &offlineSeconds,
			&createdAt, &lastSeenAt, &revokedAt, &rank, &loginAt, &loginUserID, &loginUsername, &loginFullName); err != nil {
			return nil, internal(err)
		}
		inst.Id = uuidBytes(id)
		inst.CpuCores, inst.MemoryBytes = uint32(cpuCores), uint64(memoryBytes)
		inst.OfflineAccess = durationpb.New(time.Duration(offlineSeconds) * time.Second)
		inst.CreatedAt, inst.LastSeenAt = timestamppb.New(createdAt), timestamppb.New(lastSeenAt)
		if revokedAt.Valid {
			inst.RevokedAt = timestamppb.New(revokedAt.Time)
		}
		if loginAt.Valid {
			inst.LastLoginAt = timestamppb.New(loginAt.Time)
			inst.LastLoginUser = &dentalv1.User{Id: uuidBytes(loginUserID), Username: loginUsername.String, FullName: loginFullName.String}
		}
		inst.OverDeviceLimit = rank.Valid && rank.Int64 > int64(sub.GetMaxDevicesPerClinic())
		c := byClinic[clinicID]
		c.Installations = append(c.Installations, inst)
	}
	if err := installations.Err(); err != nil {
		return nil, internal(err)
	}
	return clinics, nil
}
