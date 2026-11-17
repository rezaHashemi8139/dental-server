-- Clinics and their owners. Every installation registered from now on belongs to
-- a clinic, and the person responsible for the clinic is identified by a national ID.

-- +goose Up

-- Iranian national ID: 10 digits (the check digit is verified by the server).
-- Optional for ordinary accounts, required for clinic owners. Unique when set.
ALTER TABLE users
    ADD COLUMN national_id text,
    ADD COLUMN phone       text NOT NULL DEFAULT '',
    ADD CONSTRAINT users_national_id_format CHECK (national_id ~ '^[0-9]{10}$');
CREATE UNIQUE INDEX users_national_id_key ON users (national_id);

CREATE TABLE clinics (
    id             uuid        PRIMARY KEY DEFAULT uuidv7(),
    name           text        NOT NULL,
    -- Operating license number, unique across clinics.
    license_number text        NOT NULL,
    phone          text        NOT NULL,
    city           text        NOT NULL,
    address        text        NOT NULL,
    postal_code    text        NOT NULL DEFAULT '',
    -- The person responsible for the clinic.
    owner_id       uuid        NOT NULL REFERENCES users (id),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT clinics_license_number_format CHECK (license_number ~ '^[A-Za-z0-9./-]{3,32}$')
);
-- License numbers are not case-sensitive.
CREATE UNIQUE INDEX clinics_license_number_key ON clinics (lower(license_number));
CREATE INDEX clinics_owner_id_idx ON clinics (owner_id);

-- NULL only for installations registered before clinics existed.
ALTER TABLE installations ADD COLUMN clinic_id uuid REFERENCES clinics (id);
CREATE INDEX installations_clinic_id_idx ON installations (clinic_id);

-- +goose Down
ALTER TABLE installations DROP COLUMN clinic_id;
DROP TABLE clinics;
ALTER TABLE users DROP COLUMN phone, DROP COLUMN national_id;
