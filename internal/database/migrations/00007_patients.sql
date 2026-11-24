-- Patients and their treatments, synced from the installations (SyncService), and
-- the log of who looked at a patient's history (PatientService).
--
-- Patients are people, unique across the system by identity: an Iranian national
-- ID, or the code a foreign national was given instead. Treatments belong to the
-- clinic that recorded them. Financial data (fees, payments) is never stored here:
-- it stays in each installation's local database.

-- +goose Up

-- Every synced write takes the next value, so an installation can ask for
-- "everything changed after revision N". Writes take it inside one global lock
-- (see internal/service/sync.go), so revisions become visible in order.
CREATE SEQUENCE sync_revision_seq;

CREATE TABLE patients (
    -- Derived from the identity by the clients (see Patient.id in proto/dental/v1/patient.proto),
    -- so every installation gives the same person the same id, even offline.
    id            uuid        PRIMARY KEY,
    -- 'national' (Iranian national ID) or 'foreign'.
    identity_type text        NOT NULL,
    identity      text        NOT NULL,
    first_name    text        NOT NULL,
    last_name     text        NOT NULL,
    -- Gregorian "YYYY-MM-DD", or '' if unknown.
    birth_date    text        NOT NULL DEFAULT '',
    -- '', 'male' or 'female'.
    gender        text        NOT NULL DEFAULT '',
    phone         text        NOT NULL DEFAULT '',
    address       text        NOT NULL DEFAULT '',
    medical_notes text        NOT NULL DEFAULT '',
    -- When the details were last changed, on the client's clock (never later than
    -- the server's): the latest change wins.
    updated_at    timestamptz NOT NULL,
    -- Who sent the latest change, from which clinic.
    updated_by    uuid        REFERENCES users (id) ON DELETE SET NULL,
    updated_in    uuid        REFERENCES clinics (id) ON DELETE SET NULL,
    revision      bigint      NOT NULL DEFAULT nextval('sync_revision_seq'),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT patients_identity_type CHECK (identity_type IN ('national', 'foreign')),
    CONSTRAINT patients_identity_format CHECK (identity ~ '^[A-Z0-9]{5,20}$'),
    CONSTRAINT patients_gender CHECK (gender IN ('', 'male', 'female'))
);
CREATE UNIQUE INDEX patients_identity_key ON patients (identity_type, identity);
CREATE INDEX patients_revision_idx ON patients (revision);

-- Which clinics a patient's record is synced to: every clinic that sent it, or
-- that recorded a treatment for them.
CREATE TABLE clinic_patients (
    clinic_id  uuid        NOT NULL REFERENCES clinics (id) ON DELETE CASCADE,
    patient_id uuid        NOT NULL REFERENCES patients (id) ON DELETE CASCADE,
    -- Set when the link is made: the clinic must then receive the patient even if
    -- the patient's own revision is older than what it already pulled.
    revision   bigint      NOT NULL DEFAULT nextval('sync_revision_seq'),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (clinic_id, patient_id)
);
CREATE INDEX clinic_patients_patient_id_idx ON clinic_patients (patient_id);
CREATE INDEX clinic_patients_revision_idx ON clinic_patients (clinic_id, revision);

CREATE TABLE treatments (
    -- Random, chosen by the installation that recorded it.
    id           uuid        PRIMARY KEY,
    patient_id   uuid        NOT NULL REFERENCES patients (id) ON DELETE CASCADE,
    -- The clinic that recorded it: only its installations receive or change it.
    clinic_id    uuid        NOT NULL REFERENCES clinics (id) ON DELETE CASCADE,
    -- The account of who performed it; need not be a member of the clinic.
    performed_by uuid        REFERENCES users (id) ON DELETE SET NULL,
    performed_at timestamptz NOT NULL,
    -- e.g. FDI "36"; '' if not about one tooth.
    tooth        text        NOT NULL DEFAULT '',
    procedure    text        NOT NULL,
    notes        text        NOT NULL DEFAULT '',
    -- Same rule as patients.updated_at.
    updated_at   timestamptz NOT NULL,
    -- Kept as a tombstone, so every installation of the clinic learns about the deletion.
    deleted_at   timestamptz,
    -- Who sent the latest change.
    updated_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    revision     bigint      NOT NULL DEFAULT nextval('sync_revision_seq'),
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX treatments_patient_id_idx ON treatments (patient_id, performed_at DESC);
CREATE INDEX treatments_revision_idx ON treatments (clinic_id, revision);

-- Every look at a patient's history across clinics (PatientService.GetPatientHistory).
-- Later this will also record the patient's consent.
CREATE TABLE patient_history_views (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    patient_id      uuid        NOT NULL REFERENCES patients (id) ON DELETE CASCADE,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    clinic_id       uuid        NOT NULL REFERENCES clinics (id) ON DELETE CASCADE,
    installation_id uuid        NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    viewed_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX patient_history_views_patient_id_idx ON patient_history_views (patient_id, viewed_at DESC);

-- +goose Down
DROP TABLE patient_history_views;
DROP TABLE treatments;
DROP TABLE clinic_patients;
DROP TABLE patients;
DROP SEQUENCE sync_revision_seq;
