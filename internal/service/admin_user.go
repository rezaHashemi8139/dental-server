package service

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
)

// Listing the accounts of the desktop app. GetUser (admin.go) gives one in full.

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
		var owned, member int64
		var planName pgtype.Text
		var lastLogin pgtype.Timestamptz
		item := &dentalv1.UserListItem{User: &dentalv1.User{}, Identity: &dentalv1.PersonIdentity{}}
		if err := rows.Scan(&id, &item.User.Username, &item.User.FullName, &item.Identity.Number, &item.Phone,
			&item.Email, &item.Disabled, &createdAt, &owned, &member, &planName, &lastLogin); err != nil {
			return nil, internal(err)
		}
		item.User.Id = uuidBytes(id)
		item.CreatedAt = timestamppb.New(createdAt)
		item.OwnedClinicCount, item.MemberClinicCount = uint32(owned), uint32(member)
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
