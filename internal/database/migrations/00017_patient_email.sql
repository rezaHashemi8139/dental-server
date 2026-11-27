-- Optional email address of a patient, synced like the phone (Patient.email in
-- proto/dental/v1/patient.proto, whose rule checks the format). Empty when none
-- was given.

-- +goose Up

ALTER TABLE patients
    ADD COLUMN email text NOT NULL DEFAULT '',
    ADD CONSTRAINT patients_email_length CHECK (char_length(email) <= 254);

-- +goose Down

ALTER TABLE patients DROP COLUMN email;
