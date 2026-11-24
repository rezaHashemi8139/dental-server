package service

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/protobuf/types/known/timestamppb"

	dentalv1 "dental-server/gen/dental/v1"
	"dental-server/gen/dental/v1/dentalv1connect"
	"dental-server/internal/secure"
)

type InstallationService struct {
	d *Deps
}

var _ dentalv1connect.InstallationServiceHandler = (*InstallationService)(nil)

func NewInstallationService(d *Deps) *InstallationService {
	return &InstallationService{d: d}
}

// Register creates the owner's account (if new), the clinic (if new) and the
// installation in one transaction: either all of them exist afterwards or none does.
// Request fields are checked by ValidateInterceptor against proto/dental/v1/installation.proto.
func (s *InstallationService) Register(ctx context.Context, req *dentalv1.RegisterRequest) (*dentalv1.RegisterResponse, error) {
	m := req.GetMachine()

	// Only the owner of an existing clinic can add an installation to it.
	if req.GetExistingClinic() != nil && req.GetNewOwner() != nil {
		return nil, fail(connect.CodeInvalidArgument, reasonInvalidArgument)
	}
	identity := req.GetNewOwner().GetIdentity()
	if req.GetExistingOwner() != nil {
		identity = req.GetExistingOwner().GetIdentity()
	}
	if err := checkPersonIdentity(identity); err != nil {
		return nil, err
	}

	tx, err := s.d.DB.Begin(ctx)
	if err != nil {
		return nil, internal(err)
	}
	defer tx.Rollback(ctx)

	var owner *dentalv1.User
	var ownerID pgtype.UUID
	if newOwner := req.GetNewOwner(); newOwner != nil {
		owner, ownerID, err = createOwner(ctx, tx, newOwner)
	} else {
		owner, ownerID, err = checkExistingOwner(ctx, tx, req.GetExistingOwner())
	}
	if err != nil {
		return nil, err
	}

	var clinicID pgtype.UUID
	if c := req.GetClinic(); c != nil {
		clinicID, err = createClinic(ctx, tx, c, ownerID)
	} else {
		clinicID, err = ownedClinic(ctx, tx, req.GetExistingClinic().GetLicenseNumber(), ownerID)
	}
	if err != nil {
		return nil, err
	}

	secret := secure.RandomBytes(32)
	var id pgtype.UUID
	var known bool
	err = tx.QueryRow(ctx, `
		WITH previous AS (SELECT 1 FROM installations WHERE machine_id_hash = $1 LIMIT 1)
		INSERT INTO installations (secret_hash, machine_id_hash, hostname, os_name, os_version,
		                           cpu_model, cpu_cores, memory_bytes, mac_addresses, app_version,
		                           clinic_id)
		VALUES ($2, $1, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, EXISTS (SELECT 1 FROM previous)`,
		m.GetMachineIdHash(), secure.HashSecret(secret),
		limit(m.GetHostname(), 255), limit(m.GetOsName(), 255), limit(m.GetOsVersion(), 64),
		limit(m.GetCpuModel(), 255), int32(min(m.GetCpuCores(), 1<<16)), int64(min(m.GetMemoryBytes(), 1<<62)),
		m.GetMacAddresses(), limit(m.GetAppVersion(), 64), clinicID,
	).Scan(&id, &known)
	if err != nil {
		return nil, internal(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, internal(err)
	}

	return &dentalv1.RegisterResponse{
		InstallationId:     uuidBytes(id),
		InstallationSecret: secret,
		KnownMachine:       known,
		ServerTime:         timestamppb.New(s.d.Now()),
		Owner:              owner,
		ClinicId:           uuidBytes(clinicID),
	}, nil
}

// createOwner creates the account of a clinic owner who has none yet.
func createOwner(ctx context.Context, tx pgx.Tx, o *dentalv1.NewOwner) (*dentalv1.User, pgtype.UUID, error) {
	return insertUser(ctx, tx, account{
		Username: o.GetUsername(),
		Password: o.GetPassword(),
		FullName: o.GetFullName(),
		Identity: o.GetIdentity(),
		Phone:    o.GetPhone(),
	})
}

// createClinic creates a clinic whose owner is a member and admin from the start,
// so their first login needs nothing else.
func createClinic(ctx context.Context, tx pgx.Tx, c *dentalv1.ClinicInfo, ownerID pgtype.UUID) (pgtype.UUID, error) {
	var id pgtype.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO clinics (name, license_number, phone, city, address, postal_code, owner_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		c.GetName(), c.GetLicenseNumber(), c.GetPhone(), c.GetCity(), c.GetAddress(), c.GetPostalCode(), ownerID,
	).Scan(&id)
	if uniqueViolation(err, "clinics_license_number_key") {
		return id, fail(connect.CodeAlreadyExists, reasonLicenseNumberTaken)
	}
	if err != nil {
		return id, internal(err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO clinic_members (clinic_id, user_id, is_admin, added_by)
		VALUES ($1, $2, true, $2)`, id, ownerID); err != nil {
		return id, internal(err)
	}
	return id, nil
}

// ownedClinic finds the clinic with a license number, which must belong to ownerID.
func ownedClinic(ctx context.Context, tx pgx.Tx, licenseNumber string, ownerID pgtype.UUID) (pgtype.UUID, error) {
	var id, owner pgtype.UUID
	err := tx.QueryRow(ctx, `
		SELECT id, owner_id FROM clinics WHERE lower(license_number) = lower($1)`, licenseNumber,
	).Scan(&id, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, fail(connect.CodeNotFound, reasonClinicNotFound)
	}
	if err != nil {
		return id, internal(err)
	}
	if owner != ownerID {
		return id, fail(connect.CodePermissionDenied, reasonNotClinicOwner)
	}
	return id, nil
}

// checkExistingOwner checks the credentials and identity of an existing account,
// saving the identity if the account has none yet.
func checkExistingOwner(ctx context.Context, tx pgx.Tx, o *dentalv1.ExistingOwner) (*dentalv1.User, pgtype.UUID, error) {
	// Unknown users are checked against a dummy hash so timing does not reveal them (as in Login).
	user := &dentalv1.User{}
	var id pgtype.UUID
	var passwordHash, identityType string
	var identity pgtype.Text
	var disabledAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `
		SELECT id, username, full_name, password_hash, identity_type, identity, disabled_at
		FROM users WHERE lower(username) = lower($1) FOR UPDATE`, o.GetUsername(),
	).Scan(&id, &user.Username, &user.FullName, &passwordHash, &identityType, &identity, &disabledAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, id, internal(err)
	}
	if !secure.VerifyPassword(o.GetPassword(), passwordHash) {
		return nil, id, fail(connect.CodeUnauthenticated, reasonInvalidCredentials)
	}
	if disabledAt.Valid {
		return nil, id, fail(connect.CodePermissionDenied, reasonUserDisabled)
	}

	given := o.GetIdentity()
	switch {
	case !identity.Valid:
		_, err := tx.Exec(ctx, `
			UPDATE users SET identity_type = $2, identity = $3, nationality = $4, updated_at = now() WHERE id = $1`,
			id, identityTypeNames[given.GetType()], given.GetNumber(), given.GetNationality())
		if uniqueViolation(err, "users_identity_key") {
			return nil, id, fail(connect.CodeAlreadyExists, reasonIdentityTaken)
		}
		if err != nil {
			return nil, id, internal(err)
		}
	case identityType != identityTypeNames[given.GetType()] || identity.String != given.GetNumber():
		return nil, id, fail(connect.CodeFailedPrecondition, reasonIdentityMismatch)
	}

	user.Id = uuidBytes(id)
	return user, id, nil
}

// uniqueViolation reports whether err is a unique violation of the named constraint or index.
func uniqueViolation(err error, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// validNationalID checks an Iranian national ID: 10 digits, the last one a check
// digit. IDs made of one repeated digit pass the check but are never issued.
func validNationalID(id string) bool {
	if len(id) != 10 || strings.Trim(id, "0123456789") != "" || id == strings.Repeat(id[:1], 10) {
		return false
	}
	sum := 0
	for i := range 9 {
		sum += int(id[i]-'0') * (10 - i)
	}
	check := int(id[9] - '0')
	if r := sum % 11; r < 2 {
		return check == r
	} else {
		return check == 11-r
	}
}

// limit cuts s to at most n bytes without splitting a UTF-8 character.
func limit(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
