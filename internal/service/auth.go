package service

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
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

const databaseKeyLength = 32

// Request fields (username format, password policy, lengths) are checked by
// ValidateInterceptor against the rules in proto/dental/v1/auth.proto.

func (s *AuthService) SignUp(ctx context.Context, req *dentalv1.SignUpRequest) (*dentalv1.SignUpResponse, error) {
	user, _, err := insertUser(ctx, s.d.DB, account{
		Username: req.GetUsername(),
		Password: req.GetPassword(),
		FullName: req.GetFullName(),
		Identity: req.GetIdentity(),
		Phone:    req.GetPhone(),
		Email:    req.GetEmail(),
	})
	if err != nil {
		return nil, err
	}
	return &dentalv1.SignUpResponse{User: user}, nil
}

// account is a new user account. Its fields are already checked.
type account struct {
	Username string
	Password string
	FullName string
	Identity *dentalv1.PersonIdentity
	Phone    string
	Email    string
	// Set when someone else creates the account: the password is then temporary
	// and must be changed before the first login.
	CreatedBy pgtype.UUID
}

// insertUser creates an account. Fails with username_taken or identity_taken.
func insertUser(ctx context.Context, q querier, a account) (*dentalv1.User, pgtype.UUID, error) {
	var id pgtype.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, full_name, identity, phone, email, must_change_password, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		a.Username, secure.HashPassword(a.Password), a.FullName, a.Identity.GetNumber(), a.Phone, a.Email,
		a.CreatedBy.Valid, a.CreatedBy,
	).Scan(&id)
	switch {
	case uniqueViolation(err, "users_username_key"):
		return nil, id, fail(connect.CodeAlreadyExists, reasonUsernameTaken)
	case uniqueViolation(err, "users_identity_key"):
		return nil, id, fail(connect.CodeAlreadyExists, reasonIdentityTaken)
	case err != nil:
		return nil, id, internal(err)
	}
	return &dentalv1.User{Id: uuidBytes(id), Username: a.Username, FullName: a.FullName}, id, nil
}

func (s *AuthService) Login(ctx context.Context, req *dentalv1.LoginRequest) (*dentalv1.LoginResponse, error) {
	installationID, ok := toUUID(req.GetInstallationId())
	if !ok {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	now := s.d.Now()

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// The installation and its clinic, locked so two first logins cannot both create the keys.
	var secretHash []byte
	var clinicID, ownerID pgtype.UUID
	var offlineSeconds int64
	var revokedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
		SELECT i.secret_hash, i.clinic_id, c.owner_id, extract(epoch FROM i.offline_access)::bigint, i.revoked_at
		FROM installations i LEFT JOIN clinics c ON c.id = i.clinic_id
		WHERE i.id = $1 FOR UPDATE OF i`, installationID,
	).Scan(&secretHash, &clinicID, &ownerID, &offlineSeconds, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !secure.SecretMatches(req.GetInstallationSecret(), secretHash)) {
		return nil, fail(connect.CodeUnauthenticated, reasonInvalidInstallation)
	}
	if err != nil {
		return nil, internal(err)
	}
	if revokedAt.Valid {
		return nil, fail(connect.CodePermissionDenied, reasonInstallationRevoked)
	}
	if !clinicID.Valid {
		return nil, fail(connect.CodeFailedPrecondition, reasonNoClinic)
	}

	// The user. Unknown users are checked against a dummy hash so timing does not reveal them.
	var user dentalv1.User
	var userID pgtype.UUID
	var passwordHash string
	var disabledAt pgtype.Timestamptz
	var mustChangePassword bool
	err = tx.QueryRow(ctx, `
		SELECT id, username, full_name, password_hash, disabled_at, must_change_password
		FROM users WHERE lower(username) = lower($1)`, req.GetUsername(),
	).Scan(&userID, &user.Username, &user.FullName, &passwordHash, &disabledAt, &mustChangePassword)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, internal(err)
	}
	if !secure.VerifyPassword(req.GetPassword(), passwordHash) {
		return nil, fail(connect.CodeUnauthenticated, reasonInvalidCredentials)
	}
	if disabledAt.Valid {
		return nil, fail(connect.CodePermissionDenied, reasonUserDisabled)
	}
	if mustChangePassword {
		return nil, fail(connect.CodeFailedPrecondition, reasonPasswordChangeRequired)
	}
	user.Id = uuidBytes(userID)

	// Membership of the clinic (Register makes the owner a member), then the owner's subscription.
	var isAdmin bool
	err = tx.QueryRow(ctx, `
		SELECT is_admin FROM clinic_members WHERE clinic_id = $1 AND user_id = $2`, clinicID, userID,
	).Scan(&isAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fail(connect.CodePermissionDenied, reasonNotAMember)
	}
	if err != nil {
		return nil, internal(err)
	}
	paid, err := checkClinicAccess(ctx, tx, ownerID, clinicID, installationID, now)
	if err != nil {
		// The owner can fix this by buying: they get a billing-only session instead.
		reason, ok := subscriptionProblem(err)
		if !ok || ownerID != userID {
			return nil, err
		}
		return s.billingOnlyLogin(ctx, tx, &user, userID, installationID, isAdmin, reason, now)
	}

	// Offline access never outlasts the paid time.
	grantExpires := now.Add(time.Duration(offlineSeconds) * time.Second)
	if paid.EndsAt.Before(grantExpires) {
		grantExpires = paid.EndsAt
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

	sealedKeys, grant, err := s.sealAndSign(installationID, &user, isAdmin, keys, now, grantExpires, req.GetClientPublicKey())
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}

	return &dentalv1.LoginResponse{
		SessionToken:       token,
		SessionExpiresAt:   timestamppb.New(sessionExpires),
		User:               &user,
		IsOwner:            ownerID == userID,
		IsAdmin:            isAdmin,
		DatabaseKeys:       sealedKeys,
		OfflineGrant:       grant,
		ServerTime:         timestamppb.New(now),
		SubscriptionEndsAt: timestamppb.New(paid.EndsAt),
	}, nil
}

// subscriptionProblem reports whether err is a checkClinicAccess failure that a
// purchase fixes, and its reason.
func subscriptionProblem(err error) (string, bool) {
	connectErr, ok := errors.AsType[*connect.Error](err)
	if !ok || connectErr.Code() != connect.CodePermissionDenied {
		return "", false
	}
	switch reason := connectErr.Message(); reason {
	case reasonSubscriptionInactive, reasonClinicLimitExceeded, reasonDeviceLimitExceeded:
		return reason, true
	default:
		return "", false
	}
}

// billingOnlyLogin ends a login whose owner's subscription does not cover the clinic
// and installation: a session for BillingService only, without the database keys
// or an offline grant, so the clinic's data stays closed until they buy.
func (s *AuthService) billingOnlyLogin(ctx context.Context, tx pgx.Tx, user *dentalv1.User, userID, installationID pgtype.UUID,
	isAdmin bool, reason string, now time.Time) (*dentalv1.LoginResponse, error) {
	token, tokenHash := secure.NewToken()
	sessionExpires := now.Add(s.d.SessionTTL)
	if _, err := tx.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, installation_id, created_at, expires_at, billing_only)
		VALUES ($1, $2, $3, $4, $5, true)`, tokenHash, userID, installationID, now, sessionExpires); err != nil {
		return nil, internal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE installations SET last_seen_at = $2 WHERE id = $1`, installationID, now); err != nil {
		return nil, internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.LoginResponse{
		SessionToken:     token,
		SessionExpiresAt: timestamppb.New(sessionExpires),
		User:             user,
		IsOwner:          true,
		IsAdmin:          isAdmin,
		ServerTime:       timestamppb.New(now),
		BillingOnly:      true,
		BillingReason:    reason,
	}, nil
}

func (s *AuthService) ChangePassword(ctx context.Context, req *dentalv1.ChangePasswordRequest) (*dentalv1.ChangePasswordResponse, error) {
	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	// Unknown users are checked against a dummy hash so timing does not reveal them (as in Login).
	var userID pgtype.UUID
	var passwordHash string
	var disabledAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
		SELECT id, password_hash, disabled_at FROM users WHERE lower(username) = lower($1) FOR UPDATE`,
		req.GetUsername(),
	).Scan(&userID, &passwordHash, &disabledAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, internal(err)
	}
	if !secure.VerifyPassword(req.GetCurrentPassword(), passwordHash) {
		return nil, fail(connect.CodeUnauthenticated, reasonInvalidCredentials)
	}
	if disabledAt.Valid {
		return nil, fail(connect.CodePermissionDenied, reasonUserDisabled)
	}
	if req.GetNewPassword() == req.GetCurrentPassword() {
		return nil, fail(connect.CodeInvalidArgument, reasonPasswordUnchanged)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE users SET password_hash = $2, must_change_password = false, updated_at = now() WHERE id = $1`,
		userID, secure.HashPassword(req.GetNewPassword())); err != nil {
		return nil, internal(err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, s.d.Now()); err != nil {
		return nil, internal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}
	return &dentalv1.ChangePasswordResponse{}, nil
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
	now, grantExpires time.Time, clientPublicKey []byte,
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
		ExpiresAt:      timestamppb.New(grantExpires),
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

func (s *AuthService) GetProfile(ctx context.Context, _ *dentalv1.GetProfileRequest) (*dentalv1.GetProfileResponse, error) {
	profile, err := loadProfile(ctx, s.d.DB, sessionFrom(ctx).UserID)
	if err != nil {
		return nil, err
	}
	return &dentalv1.GetProfileResponse{Profile: profile}, nil
}

// UpdateProfile changes the full name, phone, email and birth date; the username and
// identification code stay as they are (offline logins and patients are matched by them).
// A request without a birth date (from an older app) keeps the one on file.
func (s *AuthService) UpdateProfile(ctx context.Context, req *dentalv1.UpdateProfileRequest) (*dentalv1.UpdateProfileResponse, error) {
	userID := sessionFrom(ctx).UserID
	// nil (not sent) keeps the birth date on file.
	if _, err := s.d.DB.Exec(ctx, `
		UPDATE users SET full_name = $2, phone = $3, email = $4, birth_date = coalesce($5, birth_date), updated_at = now()
		WHERE id = $1`,
		userID, req.GetFullName(), req.GetPhone(), req.GetEmail(), req.BirthDate); err != nil {
		return nil, internal(err)
	}
	profile, err := loadProfile(ctx, s.d.DB, userID)
	if err != nil {
		return nil, err
	}
	return &dentalv1.UpdateProfileResponse{Profile: profile}, nil
}

// loadProfile reads a user's own account details.
func loadProfile(ctx context.Context, q querier, userID pgtype.UUID) (*dentalv1.Profile, error) {
	user := &dentalv1.User{Id: uuidBytes(userID)}
	var identity, phone, email, birthDate string
	err := q.QueryRow(ctx, `
		SELECT username, full_name, coalesce(identity, ''), phone, email, birth_date FROM users WHERE id = $1`, userID,
	).Scan(&user.Username, &user.FullName, &identity, &phone, &email, &birthDate)
	if errors.Is(err, pgx.ErrNoRows) {
		// The session interceptor checked the user a moment ago.
		return nil, fail(connect.CodeUnauthenticated, reasonUnauthenticated)
	}
	if err != nil {
		return nil, internal(err)
	}
	return &dentalv1.Profile{
		User:     user,
		Identity: &dentalv1.PersonIdentity{Number: identity},
		Phone:     phone,
		Email:     email,
		BirthDate: birthDate,
	}, nil
}
