package service

import (
	"context"
	"errors"

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
	var freeSubscription *dentalv1.Subscription
	if c := req.GetClinic(); c != nil {
		clinicID, err = createClinic(ctx, tx, c, ownerID)
		if err == nil {
			freeSubscription, err = grantFreePlan(ctx, tx, ownerID, s.d.Now())
		}
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
		FreeSubscription:   freeSubscription,
	}, nil
}

// ListMyClinics lists the clinics an account owns or administers, and whether it can
// add this computer to each, by the same rules as Register and AuthService.Login.
func (s *InstallationService) ListMyClinics(ctx context.Context, req *dentalv1.ListMyClinicsRequest) (*dentalv1.ListMyClinicsResponse, error) {
	a, err := checkAccount(ctx, s.d.DB, req.GetUsername(), req.GetPassword(), false)
	if err != nil {
		return nil, err
	}
	now := s.d.Now()
	res := &dentalv1.ListMyClinicsResponse{User: a.User, IdentityOnFile: a.Identity.Valid}

	// Each clinic with its owner, its installations that are not revoked, and its
	// position among the owner's clinics, oldest first (see checkClinicAccess).
	type listed struct {
		clinic  *dentalv1.MyClinic
		ownerID pgtype.UUID
		rank    int64
	}
	var clinics []listed
	rows, err := s.d.DB.Query(ctx, `
		SELECT c.id, c.name, c.license_number, c.city, coalesce(c.city_id, 0), c.address, c.owner_id, o.full_name,
		       (SELECT count(*) FROM installations i WHERE i.clinic_id = c.id AND i.revoked_at IS NULL),
		       (SELECT count(*) FROM clinics c2
		        WHERE c2.owner_id = c.owner_id AND (c2.created_at, c2.id) <= (c.created_at, c.id))
		FROM clinic_members m
		JOIN clinics c ON c.id = m.clinic_id
		JOIN users o ON o.id = c.owner_id
		WHERE m.user_id = $1 AND m.is_admin
		ORDER BY c.owner_id = $1 DESC, c.created_at, c.id`, a.ID)
	if err != nil {
		return nil, internal(err)
	}
	for rows.Next() {
		var id pgtype.UUID
		var cityID int32
		var installations int64
		l := listed{clinic: &dentalv1.MyClinic{}}
		if err := rows.Scan(&id, &l.clinic.Name, &l.clinic.LicenseNumber, &l.clinic.City, &cityID, &l.clinic.Address,
			&l.ownerID, &l.clinic.OwnerFullName, &installations, &l.rank); err != nil {
			rows.Close()
			return nil, internal(err)
		}
		l.clinic.Id, l.clinic.IsOwner, l.clinic.InstallationCount = uuidBytes(id), l.ownerID == a.ID, uint32(installations)
		l.clinic.CityId = uint32(cityID)
		clinics = append(clinics, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}

	// What each owner's subscription in force allows; ok = false if none.
	type paidStatus struct {
		paid paidTime
		ok   bool
	}
	paidByOwner := map[pgtype.UUID]paidStatus{}
	paidOf := func(ownerID pgtype.UUID) (paidStatus, error) {
		if p, found := paidByOwner[ownerID]; found {
			return p, nil
		}
		p, ok, err := paidTimeAt(ctx, s.d.DB, ownerID, now)
		if err != nil {
			return paidStatus{}, internal(err)
		}
		paidByOwner[ownerID] = paidStatus{p, ok}
		return paidStatus{p, ok}, nil
	}

	for _, l := range clinics {
		p, err := paidOf(l.ownerID)
		if err != nil {
			return nil, err
		}
		c := l.clinic
		if p.ok {
			c.MaxInstallations, c.SubscriptionEndsAt = uint32(p.paid.MaxDevicesPerClinic), timestamppb.New(p.paid.EndsAt)
		}
		switch {
		case !c.IsOwner:
			c.CannotAddReason = reasonNotClinicOwner
		case !p.ok:
			c.CannotAddReason = reasonSubscriptionInactive
		case l.rank > int64(p.paid.MaxClinics):
			c.CannotAddReason = reasonClinicLimitExceeded
		case int64(c.InstallationCount) >= int64(p.paid.MaxDevicesPerClinic):
			c.CannotAddReason = reasonDeviceLimitExceeded
		}
		res.Clinics = append(res.Clinics, c)
	}

	// The account's own subscription, for a clinic it would add.
	own, err := paidOf(a.ID)
	if err != nil {
		return nil, err
	}
	if own.ok {
		res.SubscriptionEndsAt, res.MaxClinics = timestamppb.New(own.paid.EndsAt), uint32(own.paid.MaxClinics)
	}
	var owned int64
	if err := s.d.DB.QueryRow(ctx, `SELECT count(*) FROM clinics WHERE owner_id = $1`, a.ID).Scan(&owned); err != nil {
		return nil, internal(err)
	}
	res.OwnedClinicCount = uint32(owned)
	return res, nil
}

// ListCities returns every province with its cities, ordered by ID.
func (s *InstallationService) ListCities(ctx context.Context, _ *dentalv1.ListCitiesRequest) (*dentalv1.ListCitiesResponse, error) {
	rows, err := s.d.DB.Query(ctx, `
		SELECT p.id, p.name, c.id, c.name
		FROM provinces p JOIN cities c ON c.province_id = p.id
		ORDER BY p.id, c.id`)
	if err != nil {
		return nil, internal(err)
	}
	defer rows.Close()

	res := &dentalv1.ListCitiesResponse{}
	var province *dentalv1.Province
	for rows.Next() {
		var provinceID int16
		var provinceName, cityName string
		var cityID int32
		if err := rows.Scan(&provinceID, &provinceName, &cityID, &cityName); err != nil {
			return nil, internal(err)
		}
		if province == nil || province.Id != uint32(provinceID) {
			province = &dentalv1.Province{Id: uint32(provinceID), Name: provinceName}
			res.Provinces = append(res.Provinces, province)
		}
		province.Cities = append(province.Cities, &dentalv1.City{Id: uint32(cityID), Name: cityName})
	}
	if err := rows.Err(); err != nil {
		return nil, internal(err)
	}
	return res, nil
}

// createOwner creates the account of a clinic owner who has none yet.
func createOwner(ctx context.Context, tx pgx.Tx, o *dentalv1.NewOwner) (*dentalv1.User, pgtype.UUID, error) {
	return insertUser(ctx, tx, account{
		Username: o.GetUsername(),
		Password: o.GetPassword(),
		FullName: o.GetFullName(),
		Identity: o.GetIdentity(),
		Phone:    o.GetPhone(),
		Email:    o.GetEmail(),
	})
}

// createClinic creates a clinic whose owner is a member and admin from the start,
// so their first login needs nothing else.
func createClinic(ctx context.Context, tx pgx.Tx, c *dentalv1.ClinicInfo, ownerID pgtype.UUID) (pgtype.UUID, error) {
	var id pgtype.UUID
	// The city's province and name come from the list of cities (see ListCities).
	var provinceID int16
	var cityName string
	err := tx.QueryRow(ctx, `SELECT province_id, name FROM cities WHERE id = $1`, int32(c.GetCityId())).
		Scan(&provinceID, &cityName)
	if errors.Is(err, pgx.ErrNoRows) {
		return id, fail(connect.CodeNotFound, reasonCityNotFound)
	}
	if err != nil {
		return id, internal(err)
	}

	loc := c.GetLocation()
	err = tx.QueryRow(ctx, `
		INSERT INTO clinics (name, license_number, phone, province_id, city_id, city, address, postal_code,
		                     latitude, longitude, owner_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) RETURNING id`,
		c.GetName(), c.GetLicenseNumber(), c.GetPhone(), provinceID, int32(c.GetCityId()), cityName,
		c.GetAddress(), c.GetPostalCode(), loc.GetLatitude(), loc.GetLongitude(), ownerID,
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
	a, err := checkAccount(ctx, tx, o.GetUsername(), o.GetPassword(), true)
	if err != nil {
		return nil, a.ID, err
	}

	given := o.GetIdentity()
	switch {
	case given == nil && !a.Identity.Valid:
		return nil, a.ID, fail(connect.CodeFailedPrecondition, reasonIdentityRequired)
	case given == nil:
		// The identity on file stands.
	case !a.Identity.Valid:
		_, err := tx.Exec(ctx, `
			UPDATE users SET identity = $2, updated_at = now() WHERE id = $1`, a.ID, given.GetNumber())
		if uniqueViolation(err, "users_identity_key") {
			return nil, a.ID, fail(connect.CodeAlreadyExists, reasonIdentityTaken)
		}
		if err != nil {
			return nil, a.ID, internal(err)
		}
	case a.Identity.String != given.GetNumber():
		return nil, a.ID, fail(connect.CodeFailedPrecondition, reasonIdentityMismatch)
	}
	return a.User, a.ID, nil
}

// checkedAccount is an account whose password was verified.
type checkedAccount struct {
	ID   pgtype.UUID
	User *dentalv1.User
	// The identification code; NULL for accounts made before identities were required.
	Identity pgtype.Text
}

// checkAccount proves an account with its password, as AuthService.Login does, and
// fails with invalid_credentials, user_disabled or password_change_required.
// forUpdate locks the account's row until the transaction ends.
func checkAccount(ctx context.Context, q querier, username, password string, forUpdate bool) (checkedAccount, error) {
	query := `
		SELECT id, username, full_name, password_hash, identity, disabled_at, must_change_password
		FROM users WHERE lower(username) = lower($1)`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	// Unknown users are checked against a dummy hash so timing does not reveal them (as in Login).
	a := checkedAccount{User: &dentalv1.User{}}
	var passwordHash string
	var disabledAt pgtype.Timestamptz
	var mustChangePassword bool
	err := q.QueryRow(ctx, query, username).Scan(&a.ID, &a.User.Username, &a.User.FullName, &passwordHash,
		&a.Identity, &disabledAt, &mustChangePassword)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return a, internal(err)
	}
	if !secure.VerifyPassword(password, passwordHash) {
		return a, fail(connect.CodeUnauthenticated, reasonInvalidCredentials)
	}
	if disabledAt.Valid {
		return a, fail(connect.CodePermissionDenied, reasonUserDisabled)
	}
	if mustChangePassword {
		return a, fail(connect.CodeFailedPrecondition, reasonPasswordChangeRequired)
	}
	a.User.Id = uuidBytes(a.ID)
	return a, nil
}

// uniqueViolation reports whether err is a unique violation of the named constraint or index.
func uniqueViolation(err error, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
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
