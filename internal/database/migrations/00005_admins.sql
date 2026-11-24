-- System administrators, who use the admin panel. Kept apart from users, with
-- their own sessions, so no clinic account can ever act as an administrator.
-- The first one is created with `dental-server create-admin`.

-- +goose Up

CREATE TABLE admins (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    username      text        NOT NULL,
    -- Argon2id, PHC string format, as users.password_hash.
    password_hash text        NOT NULL,
    full_name     text        NOT NULL,
    -- NULL for administrators created with `dental-server create-admin`.
    created_by    uuid        REFERENCES admins (id) ON DELETE SET NULL,
    disabled_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT admins_username_format CHECK (username ~ '^[A-Za-z0-9._-]{3,32}$')
);
-- Usernames are not case-sensitive.
CREATE UNIQUE INDEX admins_username_key ON admins (lower(username));

CREATE TABLE admin_sessions (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- SHA-256 of the session token; the token itself is only on the client.
    token_hash bytea       NOT NULL UNIQUE,
    admin_id   uuid        NOT NULL REFERENCES admins (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CONSTRAINT admin_sessions_token_hash_len CHECK (octet_length(token_hash) = 32)
);
CREATE INDEX admin_sessions_admin_id_idx ON admin_sessions (admin_id);

-- +goose Down
DROP TABLE admin_sessions;
DROP TABLE admins;
