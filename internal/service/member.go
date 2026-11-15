package service

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
)

type MemberService struct {
	d *Deps
}

var _ dentalv1connect.MemberServiceHandler = (*MemberService)(nil)

func NewMemberService(d *Deps) *MemberService {
	return &MemberService{d: d}
}

const memberColumns = `
	u.id, u.username, u.full_name, i.owner_id = u.id, m.is_admin, m.created_at
	FROM installation_members m
	JOIN users u ON u.id = m.user_id
	JOIN installations i ON i.id = m.installation_id`

func scanMember(row pgx.Row) (*dentalv1.Member, error) {
	var id pgtype.UUID
	var addedAt pgtype.Timestamptz
	m := &dentalv1.Member{User: &dentalv1.User{}}
	if err := row.Scan(&id, &m.User.Username, &m.User.FullName, &m.IsOwner, &m.IsAdmin, &addedAt); err != nil {
		return nil, err
	}
	m.User.Id = uuidBytes(id)
	m.AddedAt = timestamppb.New(addedAt.Time)
	return m, nil
}

func (s *MemberService) getMember(ctx context.Context, installationID, userID pgtype.UUID) (*dentalv1.Member, error) {
	return scanMember(s.d.DB.QueryRow(ctx,
		`SELECT`+memberColumns+` WHERE m.installation_id = $1 AND m.user_id = $2`, installationID, userID))
}

func requireAdmin(ctx context.Context) (Session, error) {
	session := sessionFrom(ctx)
	if !session.IsAdmin {
		return session, fail(connect.CodePermissionDenied, reasonNotAdmin)
	}
	return session, nil
}

func (s *MemberService) ListMembers(ctx context.Context, _ *dentalv1.ListMembersRequest) (*dentalv1.ListMembersResponse, error) {
	session := sessionFrom(ctx)
	rows, err := s.d.DB.Query(ctx,
		`SELECT`+memberColumns+` WHERE m.installation_id = $1 ORDER BY m.created_at`, session.InstallationID)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	res := &dentalv1.ListMembersResponse{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, internal(err)
		}
		res.Members = append(res.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

func (s *MemberService) AddMember(ctx context.Context, req *dentalv1.AddMemberRequest) (*dentalv1.AddMemberResponse, error) {
	session, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}

	var userID pgtype.UUID
	err = s.d.DB.QueryRow(ctx, `SELECT id FROM users WHERE lower(username) = lower($1)`, req.GetUsername()).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}

	tag, err := s.d.DB.Exec(ctx, `
		INSERT INTO installation_members (installation_id, user_id, is_admin, added_by)
		VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
		session.InstallationID, userID, req.GetIsAdmin(), session.UserID)
	if err != nil {
		return nil, internal(err)
	}
	if tag.RowsAffected() == 0 {
		return nil, fail(connect.CodeAlreadyExists, reasonAlreadyMember)
	}

	member, err := s.getMember(ctx, session.InstallationID, userID)
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.AddMemberResponse{Member: member}, nil
}

func (s *MemberService) RemoveMember(ctx context.Context, req *dentalv1.RemoveMemberRequest) (*dentalv1.RemoveMemberResponse, error) {
	session, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	userID, ok := toUUID(req.GetUserId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	member, err := s.getMember(ctx, session.InstallationID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	if member.GetIsOwner() {
		return nil, fail(connect.CodeFailedPrecondition, reasonCannotRemoveOwner)
	}

	// Removing membership also ends the user's sessions on this installation
	// (SessionInterceptor requires membership).
	if _, err := s.d.DB.Exec(ctx,
		`DELETE FROM installation_members WHERE installation_id = $1 AND user_id = $2`,
		session.InstallationID, userID); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.RemoveMemberResponse{}, nil
}

func (s *MemberService) SetMemberAdmin(ctx context.Context, req *dentalv1.SetMemberAdminRequest) (*dentalv1.SetMemberAdminResponse, error) {
	session, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	userID, ok := toUUID(req.GetUserId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	member, err := s.getMember(ctx, session.InstallationID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	if member.GetIsOwner() && !req.GetIsAdmin() {
		return nil, fail(connect.CodeFailedPrecondition, reasonOwnerAlwaysAdmin)
	}

	if _, err := s.d.DB.Exec(ctx,
		`UPDATE installation_members SET is_admin = $3 WHERE installation_id = $1 AND user_id = $2`,
		session.InstallationID, userID, req.GetIsAdmin()); err != nil {
		return nil, internal(err)
	}
	member.IsAdmin = req.GetIsAdmin()
	return &dentalv1.SetMemberAdminResponse{Member: member}, nil
}
