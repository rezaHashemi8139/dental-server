package service

import (
	"context"
	"errors"
	"regexp"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

type AuthService struct {
	d *Deps
}

var _ dentalv1connect.AuthServiceHandler = (*AuthService)(nil)

func NewAuthService(d *Deps) *AuthService {
	return &AuthService{d: d}
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{3,32}$`)

const (
	minPasswordLength = 10
	maxPasswordLength = 256
	maxFullNameLength = 100
	databaseKeyLength = 32
)

func (s *AuthService) SignUp(ctx context.Context, req *dentalv1.SignUpRequest) (*dentalv1.SignUpResponse, error) {
	passwordLength := utf8.RuneCountInString(req.GetPassword())
	if !usernamePattern.MatchString(req.GetUsername()) ||
		passwordLength < minPasswordLength || passwordLength > maxPasswordLength ||
		utf8.RuneCountInString(req.GetFullName()) > maxFullNameLength {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	var id pgtype.UUID
	err := s.d.DB.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, full_name) VALUES ($1, $2, $3) RETURNING id`,
		req.GetUsername(), secure.HashPassword(req.GetPassword()), req.GetFullName(),
	).Scan(&id)
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "23505" {
		return nil, fail(connect.CodeAlreadyExists, reasonUsernameTaken)
	}
	if err != nil {
		return nil, internal(err)
	}

	return &dentalv1.SignUpResponse{User: &dentalv1.User{
		Id:       uuidBytes(id),
		Username: req.GetUsername(),
		FullName: req.GetFullName(),
	}}, nil
}

func (s *AuthService) Login(ctx context.Context, req *dentalv1.LoginRequest) (*dentalv1.LoginResponse, error) {
	installationID, ok := toUUID(req.GetInstallationId())
	if !ok || len(req.GetInstallationSecret()) != 32 || len(req.GetClientPublicKey()) != 32 ||
		req.GetUsername() == "" || len(req.GetPassword()) > 4*maxPasswordLength {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	now := s.d.Now()

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// The installation, locked so two first logins cannot both become owner.
	var secretHash []byte
	var owner pgtype.UUID
	var offlineSeconds int64
	var revokedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
		SELECT secret_hash, owner_id, extract(epoch FROM offline_access)::bigint, revoked_at
		FROM installations WHERE id = $1 FOR UPDATE`, installationID,
	).Scan(&secretHash, &owner, &offlineSeconds, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !secure.SecretMatches(req.GetInstallationSecret(), secretHash)) {
		return nil, fail(connect.CodeUnauthenticated, reasonInvalidInstallation)
	}
	if err != nil {
		return nil, internal(err)
	}
	if revokedAt.Valid {
		return nil, fail(connect.CodePermissionDenied, reasonInstallationRevoked)
	}

	// The user. Unknown users are checked against a dummy hash so timing does not reveal them.
	var user dentalv1.User
	var userID pgtype.UUID
	var passwordHash string
	var disabledAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
		SELECT id, username, full_name, password_hash, disabled_at
		FROM users WHERE lower(username) = lower($1)`, req.GetUsername(),
	).Scan(&userID, &user.Username, &user.FullName, &passwordHash, &disabledAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, internal(err)
	}
	if !secure.VerifyPassword(req.GetPassword(), passwordHash) {
		return nil, fail(connect.CodeUnauthenticated, reasonInvalidCredentials)
	}
	if disabledAt.Valid {
		return nil, fail(connect.CodePermissionDenied, reasonUserDisabled)
	}
	user.Id = uuidBytes(userID)

	// Membership. The first user to log in on a new installation becomes its owner.
	isOwner := !owner.Valid || owner == userID
	var isAdmin bool
	if !owner.Valid {
		if _, err := tx.Exec(ctx, `UPDATE installations SET owner_id = $2 WHERE id = $1`, installationID, userID); err != nil {
			return nil, internal(err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO installation_members (installation_id, user_id, is_admin, added_by)
			VALUES ($1, $2, true, $2)`, installationID, userID); err != nil {
			return nil, internal(err)
		}
		isAdmin = true
	} else {
		err := tx.QueryRow(ctx, `
			SELECT is_admin FROM installation_members WHERE installation_id = $1 AND user_id = $2`,
			installationID, userID,
		).Scan(&isAdmin)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fail(connect.CodePermissionDenied, reasonNotAMember)
		}
		if err != nil {
			return nil, internal(err)
		}
	}

	keys, err := s.prepareKeys(ctx, tx, installationID)
	if err != nil {
		return nil, err
	}
	defer secure.Wipe(keys.CurrentKey)
	defer secure.Wipe(keys.NextKey)

	token, tokenHash := secure.NewToken()
	sessionExpires := now.Add(s.d.SessionTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, installation_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, tokenHash, userID, installationID, now, sessionExpires); err != nil {
		return nil, internal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE installations SET last_seen_at = $2 WHERE id = $1`, installationID, now); err != nil {
		return nil, internal(err)
	}

	sealedKeys, grant, err := s.sealAndSign(installationID, &user, isAdmin, keys, now, time.Duration(offlineSeconds)*time.Second, req.GetClientPublicKey())
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}

	return &dentalv1.LoginResponse{
		SessionToken:     token,
		SessionExpiresAt: timestamppb.New(sessionExpires),
		User:             &user,
		IsOwner:          isOwner,
		IsAdmin:          isAdmin,
		DatabaseKeys:     sealedKeys,
		OfflineGrant:     grant,
		ServerTime:       timestamppb.New(now),
	}, nil
}

// prepareKeys returns the installation's current key and a pending next key,
// creating them on first use. A pending next key is reused until confirmed, so a
// retried login after a lost confirmation gets the same pair.
func (s *AuthService) prepareKeys(ctx context.Context, tx pgx.Tx, installationID pgtype.UUID) (*dentalv1.DatabaseKeys, error) {
	aad := uuidBytes(installationID)

	var version int32
	var current, next []byte
	err := tx.QueryRow(ctx, `
		SELECT key_version, current_key, next_key FROM installation_keys
		WHERE installation_id = $1 FOR UPDATE`, installationID,
	).Scan(&version, &current, &next)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		version = 0
		if _, err := tx.Exec(ctx, `INSERT INTO installation_keys (installation_id) VALUES ($1)`, installationID); err != nil {
			return nil, internal(err)
		}
	case err != nil:
		return nil, internal(err)
	}

	if next == nil {
		nextPlain := secure.RandomBytes(databaseKeyLength)
		next = s.d.Keys.Wrap(nextPlain, aad)
		secure.Wipe(nextPlain)
		if _, err := tx.Exec(ctx, `
			UPDATE installation_keys SET next_key = $2, updated_at = now() WHERE installation_id = $1`,
			installationID, next); err != nil {
			return nil, internal(err)
		}
	}

	keys := &dentalv1.DatabaseKeys{CurrentVersion: uint32(version)}
	if current != nil {
		if keys.CurrentKey, err = s.d.Keys.Unwrap(current, aad); err != nil {
			return nil, internal(err)
		}
	}
	if keys.NextKey, err = s.d.Keys.Unwrap(next, aad); err != nil {
		return nil, internal(err)
	}
	return keys, nil
}

func (s *AuthService) sealAndSign(
	installationID pgtype.UUID, user *dentalv1.User, isAdmin bool, keys *dentalv1.DatabaseKeys,
	now time.Time, offlineAccess time.Duration, clientPublicKey []byte,
) (*dentalv1.SealedBox, *dentalv1.SignedOfflineGrant, error) {
	plainKeys, err := proto.Marshal(keys)
	if err != nil {
		return nil, nil, internal(err)
	}
	defer secure.Wipe(plainKeys)

	sealed, err := secure.Seal(clientPublicKey, plainKeys, uuidBytes(installationID))
	if err != nil {
		return nil, nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}

	grantBytes, err := proto.Marshal(&dentalv1.OfflineGrant{
		InstallationId: uuidBytes(installationID),
		UserId:         user.GetId(),
		Username:       user.GetUsername(),
		FullName:       user.GetFullName(),
		IsAdmin:        isAdmin,
		IssuedAt:       timestamppb.New(now),
		ExpiresAt:      timestamppb.New(now.Add(offlineAccess)),
	})
	if err != nil {
		return nil, nil, internal(err)
	}

	return sealed, &dentalv1.SignedOfflineGrant{Grant: grantBytes, Signature: s.d.Signer.Sign(grantBytes)}, nil
}

func (s *AuthService) ConfirmKeyRotation(ctx context.Context, req *dentalv1.ConfirmKeyRotationRequest) (*dentalv1.ConfirmKeyRotationResponse, error) {
	session := sessionFrom(ctx)

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	var version int32
	var hasNext bool
	err = tx.QueryRow(ctx, `
		SELECT key_version, next_key IS NOT NULL FROM installation_keys
		WHERE installation_id = $1 FOR UPDATE`, session.InstallationID,
	).Scan(&version, &hasNext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodeFailedPrecondition, reasonRotationMismatch)
	}
	if err != nil {
		return nil, internal(err)
	}

	switch {
	case req.GetNewVersion() == uint32(version):
		// Already confirmed (a retry after a lost response): nothing to do.
	case req.GetNewVersion() == uint32(version)+1 && hasNext:
		if _, err := tx.Exec(ctx, `
			UPDATE installation_keys
			SET current_key = next_key, next_key = NULL, key_version = key_version + 1, updated_at = now()
			WHERE installation_id = $1`, session.InstallationID); err != nil {
			return nil, internal(err)
		}
		version++
	default:
		return nil, fail(connect.CodeFailedPrecondition, reasonRotationMismatch)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.ConfirmKeyRotationResponse{CurrentVersion: uint32(version)}, nil
}

func (s *AuthService) Logout(ctx context.Context, _ *dentalv1.LogoutRequest) (*dentalv1.LogoutResponse, error) {
	session := sessionFrom(ctx)
	if _, err := s.d.DB.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE id = $1`, session.ID, s.d.Now()); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.LogoutResponse{}, nil
}
