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
	u.id, u.username, u.full_name, c.owner_id = u.id, m.is_admin, m.created_at
	FROM clinic_members m
	JOIN users u ON u.id = m.user_id
	JOIN clinics c ON c.id = m.clinic_id`

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

func (s *MemberService) getMember(ctx context.Context, clinicID, userID pgtype.UUID) (*dentalv1.Member, error) {
	return scanMember(s.d.DB.QueryRow(ctx,
		`SELECT`+memberColumns+` WHERE m.clinic_id = $1 AND m.user_id = $2`, clinicID, userID))
}

func requireAdmin(ctx context.Context) (Session, error) {
	session := sessionFrom(ctx)
	if !session.IsAdmin {
		return session, fail(connect.CodePermissionDenied, reasonNotAdmin)
	}
	return session, nil
}

// findUserByIdentity returns the account with an identification code, or pgx.ErrNoRows.
func findUserByIdentity(ctx context.Context, q querier, identity *dentalv1.PersonIdentity) (*dentalv1.User, error) {
	var id pgtype.UUID
	user := &dentalv1.User{}
	err := q.QueryRow(ctx, `
		SELECT id, username, full_name FROM users WHERE identity = $1`, identity.GetNumber(),
	).Scan(&id, &user.Username, &user.FullName)
	if err != nil {
		return nil, err
	}
	user.Id = uuidBytes(id)
	return user, nil
}

func (s *MemberService) ListMembers(ctx context.Context, _ *dentalv1.ListMembersRequest) (*dentalv1.ListMembersResponse, error) {
	session := sessionFrom(ctx)
	rows, err := s.d.DB.Query(ctx,
		`SELECT`+memberColumns+` WHERE m.clinic_id = $1 ORDER BY m.created_at`, session.ClinicID)
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

func (s *MemberService) FindUser(ctx context.Context, req *dentalv1.FindUserRequest) (*dentalv1.FindUserResponse, error) {
	if _, err := requireAdmin(ctx); err != nil {
		return nil, err
	}
	user, err := findUserByIdentity(ctx, s.d.DB, req.GetIdentity())
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.FindUserResponse{User: user}, nil
}

func (s *MemberService) CreateUser(ctx context.Context, req *dentalv1.CreateUserRequest) (*dentalv1.CreateUserResponse, error) {
	session, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	// One account per person: if another clinic already created it, that one is returned.
	existing, err := findUserByIdentity(ctx, s.d.DB, req.GetIdentity())
	if err == nil {
		return &dentalv1.CreateUserResponse{User: existing, Created: false}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, internal(err)
	}

	user, _, err := insertUser(ctx, s.d.DB, account{
		Username:  req.GetUsername(),
		Password:  req.GetTemporaryPassword(),
		FullName:  req.GetFullName(),
		Identity:  req.GetIdentity(),
		Phone:     req.GetPhone(),
		CreatedBy: session.UserID,
	})
	if err != nil {
		return nil, err
	}
	return &dentalv1.CreateUserResponse{User: user, Created: true}, nil
}

func (s *MemberService) AddMember(ctx context.Context, req *dentalv1.AddMemberRequest) (*dentalv1.AddMemberResponse, error) {
	session, err := requireAdmin(ctx)
	if err != nil {
		return nil, err
	}
	userID, ok := toUUID(req.GetUserId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	tag, err := s.d.DB.Exec(ctx, `
		INSERT INTO clinic_members (clinic_id, user_id, is_admin, added_by)
		SELECT $1, id, $3, $4 FROM users WHERE id = $2
		ON CONFLICT DO NOTHING`,
		session.ClinicID, userID, req.GetIsAdmin(), session.UserID)
	if err != nil {
		return nil, internal(err)
	}

	member, err := s.getMember(ctx, session.ClinicID, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	case err != nil:
		return nil, internal(err)
	case tag.RowsAffected() == 0:
		return nil, fail(connect.CodeAlreadyExists, reasonAlreadyMember)
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

	member, err := s.getMember(ctx, session.ClinicID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeNotFound, reasonUserNotFound)
	}
	if err != nil {
		return nil, internal(err)
	}
	if member.GetIsOwner() {
		return nil, fail(connect.CodeFailedPrecondition, reasonCannotRemoveOwner)
	}

	// Removing membership also ends the user's sessions on every installation of
	// this clinic (SessionInterceptor requires membership).
	if _, err := s.d.DB.Exec(ctx,
		`DELETE FROM clinic_members WHERE clinic_id = $1 AND user_id = $2`,
		session.ClinicID, userID); err != nil {
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

	member, err := s.getMember(ctx, session.ClinicID, userID)
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
		`UPDATE clinic_members SET is_admin = $3 WHERE clinic_id = $1 AND user_id = $2`,
		session.ClinicID, userID, req.GetIsAdmin()); err != nil {
		return nil, internal(err)
	}
	member.IsAdmin = req.GetIsAdmin()
	return &dentalv1.SetMemberAdminResponse{Member: member}, nil
}
