package service

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
)

// Listing and deleting the accounts of the desktop app. GetUser (admin.go) gives one in full.

func (s *AdminService) DeleteUser(ctx context.Context, req *dentalv1.DeleteUserRequest) (*dentalv1.DeleteUserResponse, error) {
	id, ok := toUUID(req.GetId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// Held like a synced write, so no push links a patient to a clinic while the
	// patients left in no clinic are deleted below.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(syncWriteLock)); err != nil {
		return nil, internal(err)
	}
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}

	// The patients of its clinics, to delete those that no other clinic has once the clinics are gone.
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT cp.patient_id FROM clinic_patients cp JOIN clinics c ON c.id = cp.clinic_id
		WHERE c.owner_id = $1`, id)
	if err != nil {
		return nil, internal(err)
	}
	patientIDs, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
	if err != nil {
		return nil, internal(err)
	}

	// In order, as the foreign keys require: payments name subscriptions, installations
	// name clinics, and clinics and subscriptions name the user. The rest (sessions,
	// memberships, clinic patients, treatments, history views) goes with them by cascade.
	for _, sql := range []string{
		`DELETE FROM payments WHERE user_id = $1`,
		`DELETE FROM subscriptions WHERE user_id = $1`,
		`DELETE FROM installations WHERE clinic_id IN (SELECT id FROM clinics WHERE owner_id = $1)`,
		`DELETE FROM clinics WHERE owner_id = $1`,
		`DELETE FROM users WHERE id = $1`,
	} {
		if _, err := tx.Exec(ctx, sql, id); err != nil {
			return nil, internal(err)
		}
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM patients p
		WHERE p.id = ANY($1) AND NOT EXISTS (SELECT 1 FROM clinic_patients cp WHERE cp.patient_id = p.id)`,
		patientIDs); err != nil {
		return nil, internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.DeleteUserResponse{}, nil
}

func (s *AdminService) ListUsers(ctx context.Context, req *dentalv1.ListUsersRequest) (*dentalv1.ListUsersResponse, error) {
	now := s.d.Now()
	query := searchText(req.GetQuery())
	limit, offset := pageBounds(req.GetPage(), req.GetPageSize())

	from := `
		FROM users u
		WHERE $1 = '' OR ` + ownerMatchesSQL("$1")

	res := &dentalv1.ListUsersResponse{}
	var total int64
	if err := s.d.DB.QueryRow(ctx, `SELECT count(*)`+from, query).Scan(&total); err != nil {
		return nil, internal(err)
	}
	res.TotalCount = uint32(total)

	// The plan of the subscription in force at $4 (see subscriptionStatusSQL: active).
	rows, err := s.d.DB.Query(ctx, `
		SELECT u.id, u.username, u.full_name, coalesce(u.identity, ''), u.phone, u.email,
		       u.disabled_at IS NOT NULL, u.created_at,
		       (SELECT count(*) FROM clinics c WHERE c.owner_id = u.id),
		       (SELECT count(*) FROM clinic_members m WHERE m.user_id = u.id),
		       (SELECT count(*) FROM installations i JOIN clinics c ON c.id = i.clinic_id
		        WHERE c.owner_id = u.id AND i.revoked_at IS NULL),
		       (SELECT p.name FROM subscriptions s JOIN plans p ON p.id = s.plan_id
		        WHERE s.user_id = u.id AND s.canceled_at IS NULL AND s.starts_at <= $4 AND $4 < s.ends_at
		        ORDER BY s.starts_at DESC LIMIT 1),
		       (SELECT max(se.created_at) FROM sessions se WHERE se.user_id = u.id)`+from+`
		ORDER BY u.created_at DESC, u.id DESC
		LIMIT $2 OFFSET $3`, query, limit, offset, now)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	var ids []pgtype.UUID
	for rows.Next() {
		var id pgtype.UUID
		var createdAt time.Time
		var owned, member, installations int64
		var planName pgtype.Text
		var lastLogin pgtype.Timestamptz
		item := &dentalv1.UserListItem{User: &dentalv1.User{}, Identity: &dentalv1.PersonIdentity{}}
		if err := rows.Scan(&id, &item.User.Username, &item.User.FullName, &item.Identity.Number, &item.Phone,
			&item.Email, &item.Disabled, &createdAt, &owned, &member, &installations, &planName, &lastLogin); err != nil {
			return nil, internal(err)
		}
		item.User.Id = uuidBytes(id)
		item.CreatedAt = timestamppb.New(createdAt)
		item.OwnedClinicCount, item.MemberClinicCount = uint32(owned), uint32(member)
		item.InstallationCount = uint32(installations)
		item.PlanName = planName.String
		if lastLogin.Valid {
			item.LastLoginAt = timestamppb.New(lastLogin.Time)
		}
		ids = append(ids, id)
		res.Users = append(res.Users, item)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}

	// Paid time, renewals included, for those with a subscription in force.
	for i, item := range res.Users {
		if item.PlanName == "" {
			continue
		}
		paid, ok, err := paidTimeAt(ctx, s.d.DB, ids[i], now)
		if err != nil {
			return nil, internal(err)
		}
		if ok {
			item.PaidUntil = timestamppb.New(paid.EndsAt)
		}
	}
	return res, nil
}
