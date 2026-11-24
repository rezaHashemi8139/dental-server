-- Foreign nationals. Users and patients are identified the same way (PersonIdentity
-- in proto/dental/v1/person.proto): Iranian citizens by national ID, foreign
-- nationals by the FIDA code issued to them in Iran. Everyone's nationality is
-- recorded, so it is always clear who is a foreign national.

-- +goose Up

-- users.national_id becomes users.identity, typed like patients.identity.
-- It stays NULL only for accounts created before identities were required.
ALTER TABLE users DROP CONSTRAINT users_national_id_format;
DROP INDEX users_national_id_key;
ALTER TABLE users RENAME COLUMN national_id TO identity;
ALTER TABLE users
    -- 'national' (Iranian national ID) or 'foreign' (FIDA code).
    ADD COLUMN identity_type text NOT NULL DEFAULT 'national',
    -- ISO 3166-1 alpha-2: 'IR' for national IDs, another country for foreign nationals.
    ADD COLUMN nationality   text NOT NULL DEFAULT 'IR',
    ADD CONSTRAINT users_identity_type CHECK (identity_type IN ('national', 'foreign')),
    ADD CONSTRAINT users_identity_format CHECK (
        CASE identity_type WHEN 'national' THEN identity ~ '^[0-9]{10}$' ELSE identity ~ '^[A-Z0-9]{5,20}$' END),
    ADD CONSTRAINT users_nationality CHECK (
        nationality ~ '^[A-Z]{2}$' AND (nationality = 'IR') = (identity_type = 'national'));
CREATE UNIQUE INDEX users_identity_key ON users (identity_type, identity);

ALTER TABLE patients
    -- Same rule as users.nationality.
    ADD COLUMN nationality text NOT NULL DEFAULT 'IR',
    ADD CONSTRAINT patients_nationality CHECK (
        nationality ~ '^[A-Z]{2}$' AND (nationality = 'IR') = (identity_type = 'national'));

-- +goose Down
ALTER TABLE patients DROP COLUMN nationality;

-- Fails while foreign nationals have accounts: their FIDA codes are not national IDs.
DROP INDEX users_identity_key;
ALTER TABLE users
    DROP CONSTRAINT users_nationality,
    DROP CONSTRAINT users_identity_format,
    DROP CONSTRAINT users_identity_type,
    DROP COLUMN nationality,
    DROP COLUMN identity_type;
ALTER TABLE users RENAME COLUMN identity TO national_id;
ALTER TABLE users ADD CONSTRAINT users_national_id_format CHECK (national_id ~ '^[0-9]{10}$');
CREATE UNIQUE INDEX users_national_id_key ON users (national_id);
