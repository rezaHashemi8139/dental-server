package service

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgtype"
)

// paidTime is what a clinic owner's subscriptions allow at one moment.
type paidTime struct {
	MaxClinics          int32
	MaxDevicesPerClinic int32
	// The end of the subscription in force, or of the last renewal that follows it without a gap.
	EndsAt time.Time
}

// paidTimeAt returns what the owner's subscription in force at `at` allows,
// or ok = false if none is in force.
func paidTimeAt(ctx context.Context, q querier, ownerID pgtype.UUID, at time.Time) (p paidTime, ok bool, err error) {
	rows, err := q.Query(ctx, `
		SELECT max_clinics, max_devices_per_clinic, starts_at, ends_at FROM subscriptions
		WHERE user_id = $1 AND canceled_at IS NULL AND ends_at > $2
		ORDER BY starts_at`, ownerID, at)
	if err != nil {
		return p, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var maxClinics, maxDevices int32
		var startsAt, endsAt time.Time
		if err := rows.Scan(&maxClinics, &maxDevices, &startsAt, &endsAt); err != nil {
			return p, false, err
		}
		if !ok {
			if startsAt.After(at) {
				// Only a later subscription: nothing in force now.
				return p, false, nil
			}
			p, ok = paidTime{MaxClinics: maxClinics, MaxDevicesPerClinic: maxDevices, EndsAt: endsAt}, true
			continue
		}
		if startsAt.After(p.EndsAt) {
			break
		}
		if endsAt.After(p.EndsAt) {
			p.EndsAt = endsAt
		}
	}
	return p, ok, rows.Err()
}

// checkClinicAccess checks that the owner's subscription in force covers the
// clinic and the installation, and returns what it allows. When the owner has
// more clinics than the subscription allows, the oldest ones are covered; the same
// goes for the clinic's installations that are not revoked.
func checkClinicAccess(ctx context.Context, q querier, ownerID, clinicID, installationID pgtype.UUID, now time.Time) (paidTime, error) {
	p, ok, err := paidTimeAt(ctx, q, ownerID, now)
	if err != nil {
		return p, internal(err)
	}
	if !ok {
		return p, fail(connect.CodePermissionDenied, reasonSubscriptionInactive)
	}

	// Position of the clinic among the owner's clinics, and of the installation
	// among the clinic's installations, oldest first.
	var clinicRank, installationRank int64
	err = q.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM clinics c
			 WHERE c.owner_id = $1 AND (c.created_at, c.id) <= (SELECT created_at, id FROM clinics WHERE id = $2)),
			(SELECT count(*) FROM installations i
			 WHERE i.clinic_id = $2 AND i.revoked_at IS NULL
			   AND (i.created_at, i.id) <= (SELECT created_at, id FROM installations WHERE id = $3))`,
		ownerID, clinicID, installationID,
	).Scan(&clinicRank, &installationRank)
	if err != nil {
		return p, internal(err)
	}
	if clinicRank > int64(p.MaxClinics) {
		return p, fail(connect.CodePermissionDenied, reasonClinicLimitExceeded)
	}
	if installationRank > int64(p.MaxDevicesPerClinic) {
		return p, fail(connect.CodePermissionDenied, reasonDeviceLimitExceeded)
	}
	return p, nil
}
