-- People are identified by one code only. Users and patients keep a single
-- identification code (PersonIdentity.number in proto/dental/v1/person.proto), e.g.
-- a national ID or any other official code, unique across the whole system. The
-- identity type and the nationality are no longer recorded.
--
-- Patient ids are derived from the code alone now (see Patient.id in
-- proto/dental/v1/patient.proto), so existing ids are recomputed. Their records get
-- new revisions, so every clinic pulls them again under the new ids.
--
-- Fails if two users, or two patients, had the same code with different types.

-- +goose Up

DROP INDEX users_identity_key;
ALTER TABLE users
    DROP CONSTRAINT users_nationality,
    DROP CONSTRAINT users_identity_format,
    DROP CONSTRAINT users_identity_type,
    DROP COLUMN nationality,
    DROP COLUMN identity_type,
    ADD CONSTRAINT users_identity_format CHECK (identity ~ '^[A-Z0-9]{5,20}$');
CREATE UNIQUE INDEX users_identity_key ON users (identity);

DROP INDEX patients_identity_key;
ALTER TABLE patients
    DROP CONSTRAINT patients_nationality,
    DROP CONSTRAINT patients_identity_type,
    DROP COLUMN nationality,
    DROP COLUMN identity_type;
CREATE UNIQUE INDEX patients_identity_key ON patients (identity);

-- New patient ids: the first 16 bytes of SHA-256('dental/patient/v2' 0x00 identity),
-- UUID version 8, RFC 4122 variant (patientID in internal/service/sync.go).
CREATE TEMPORARY TABLE patient_ids ON COMMIT DROP AS
SELECT id AS old_id,
       encode(set_byte(set_byte(h, 6, (get_byte(h, 6) & 15) | 128), 8, (get_byte(h, 8) & 63) | 128), 'hex')::uuid AS new_id
FROM (SELECT id,
             substring(sha256(convert_to('dental/patient/v2', 'UTF8') || '\x00'::bytea || convert_to(identity, 'UTF8'))
                       FROM 1 FOR 16) AS h
      FROM patients) p;

ALTER TABLE clinic_patients DROP CONSTRAINT clinic_patients_patient_id_fkey;
ALTER TABLE treatments DROP CONSTRAINT treatments_patient_id_fkey;
ALTER TABLE patient_history_views DROP CONSTRAINT patient_history_views_patient_id_fkey;

UPDATE patients p SET id = m.new_id, revision = nextval('sync_revision_seq')
FROM patient_ids m WHERE p.id = m.old_id;
UPDATE clinic_patients c SET patient_id = m.new_id, revision = nextval('sync_revision_seq')
FROM patient_ids m WHERE c.patient_id = m.old_id;
UPDATE treatments t SET patient_id = m.new_id, revision = nextval('sync_revision_seq')
FROM patient_ids m WHERE t.patient_id = m.old_id;
UPDATE patient_history_views v SET patient_id = m.new_id
FROM patient_ids m WHERE v.patient_id = m.old_id;

ALTER TABLE clinic_patients ADD CONSTRAINT clinic_patients_patient_id_fkey
    FOREIGN KEY (patient_id) REFERENCES patients (id) ON DELETE CASCADE;
ALTER TABLE treatments ADD CONSTRAINT treatments_patient_id_fkey
    FOREIGN KEY (patient_id) REFERENCES patients (id) ON DELETE CASCADE;
ALTER TABLE patient_history_views ADD CONSTRAINT patient_history_views_patient_id_fkey
    FOREIGN KEY (patient_id) REFERENCES patients (id) ON DELETE CASCADE;

-- +goose Down

-- The type is guessed from the code: 10 digits is a national ID, anything else is
-- foreign. Nationalities are lost: foreign nationals get 'ZZ' (unknown).
ALTER TABLE users
    DROP CONSTRAINT users_identity_format,
    ADD COLUMN identity_type text NOT NULL DEFAULT 'national',
    ADD COLUMN nationality   text NOT NULL DEFAULT 'IR';
UPDATE users SET identity_type = 'foreign', nationality = 'ZZ' WHERE identity !~ '^[0-9]{10}$';
DROP INDEX users_identity_key;
ALTER TABLE users
    ADD CONSTRAINT users_identity_type CHECK (identity_type IN ('national', 'foreign')),
    ADD CONSTRAINT users_identity_format CHECK (
        CASE identity_type WHEN 'national' THEN identity ~ '^[0-9]{10}$' ELSE identity ~ '^[A-Z0-9]{5,20}$' END),
    ADD CONSTRAINT users_nationality CHECK (
        nationality ~ '^[A-Z]{2}$' AND (nationality = 'IR') = (identity_type = 'national'));
CREATE UNIQUE INDEX users_identity_key ON users (identity_type, identity);

ALTER TABLE patients
    ADD COLUMN identity_type text NOT NULL DEFAULT 'national',
    ADD COLUMN nationality   text NOT NULL DEFAULT 'IR';
UPDATE patients SET identity_type = 'foreign', nationality = 'ZZ' WHERE identity !~ '^[0-9]{10}$';
DROP INDEX patients_identity_key;
ALTER TABLE patients
    ALTER COLUMN identity_type DROP DEFAULT,
    ADD CONSTRAINT patients_identity_type CHECK (identity_type IN ('national', 'foreign')),
    ADD CONSTRAINT patients_nationality CHECK (
        nationality ~ '^[A-Z]{2}$' AND (nationality = 'IR') = (identity_type = 'national'));
CREATE UNIQUE INDEX patients_identity_key ON patients (identity_type, identity);

-- Old patient ids: SHA-256('dental/patient/v1' 0x00 <type number> 0x00 identity),
-- where national = 1 and foreign = 2.
CREATE TEMPORARY TABLE patient_ids ON COMMIT DROP AS
SELECT id AS old_id,
       encode(set_byte(set_byte(h, 6, (get_byte(h, 6) & 15) | 128), 8, (get_byte(h, 8) & 63) | 128), 'hex')::uuid AS new_id
FROM (SELECT id,
             substring(sha256(convert_to('dental/patient/v1', 'UTF8') || '\x00'::bytea
                              || convert_to(CASE identity_type WHEN 'national' THEN '1' ELSE '2' END, 'UTF8')
                              || '\x00'::bytea || convert_to(identity, 'UTF8'))
                       FROM 1 FOR 16) AS h
      FROM patients) p;

ALTER TABLE clinic_patients DROP CONSTRAINT clinic_patients_patient_id_fkey;
ALTER TABLE treatments DROP CONSTRAINT treatments_patient_id_fkey;
ALTER TABLE patient_history_views DROP CONSTRAINT patient_history_views_patient_id_fkey;

UPDATE patients p SET id = m.new_id, revision = nextval('sync_revision_seq')
FROM patient_ids m WHERE p.id = m.old_id;
UPDATE clinic_patients c SET patient_id = m.new_id, revision = nextval('sync_revision_seq')
FROM patient_ids m WHERE c.patient_id = m.old_id;
UPDATE treatments t SET patient_id = m.new_id, revision = nextval('sync_revision_seq')
FROM patient_ids m WHERE t.patient_id = m.old_id;
UPDATE patient_history_views v SET patient_id = m.new_id
FROM patient_ids m WHERE v.patient_id = m.old_id;

ALTER TABLE clinic_patients ADD CONSTRAINT clinic_patients_patient_id_fkey
    FOREIGN KEY (patient_id) REFERENCES patients (id) ON DELETE CASCADE;
ALTER TABLE treatments ADD CONSTRAINT treatments_patient_id_fkey
    FOREIGN KEY (patient_id) REFERENCES patients (id) ON DELETE CASCADE;
ALTER TABLE patient_history_views ADD CONSTRAINT patient_history_views_patient_id_fkey
    FOREIGN KEY (patient_id) REFERENCES patients (id) ON DELETE CASCADE;
