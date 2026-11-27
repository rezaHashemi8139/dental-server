-- Optional email address of an account. Contact information only: it is not
-- unique and is not used to log in. Empty when the user gave none. The format is
-- checked by the request rules (proto/dental/v1/auth.proto, SignUpRequest.email).

-- +goose Up

ALTER TABLE users
    ADD COLUMN email text NOT NULL DEFAULT '',
    ADD CONSTRAINT users_email_length CHECK (char_length(email) <= 254);

-- +goose Down

ALTER TABLE users DROP COLUMN email;
