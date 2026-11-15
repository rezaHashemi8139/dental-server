-- Accounts, installations, membership, local-database keys and sessions.
-- Only user data lives here: patient records stay in each installation's
-- encrypted local database and never reach the server.

-- +goose Up

CREATE TABLE users (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    username      text        NOT NULL,
    -- Argon2id, PHC string format ($argon2id$v=19$m=...,t=...,p=...$salt$hash).
    password_hash text        NOT NULL,
    full_name     text        NOT NULL DEFAULT '',
    disabled_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_username_format CHECK (username ~ '^[A-Za-z0-9._-]{3,32}$')
);
-- Usernames are not case-sensitive.
CREATE UNIQUE INDEX users_username_key ON users (lower(username));

CREATE TABLE installations (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- SHA-256 of the installation secret; the secret itself is only on the client.
    secret_hash     bytea       NOT NULL,
    -- SHA-256 of the Windows MachineGuid: recognises the computer across reinstalls of the app.
    machine_id_hash bytea       NOT NULL,
    hostname        text        NOT NULL DEFAULT '',
    os_name         text        NOT NULL DEFAULT '',
    os_version      text        NOT NULL DEFAULT '',
    cpu_model       text        NOT NULL DEFAULT '',
    cpu_cores       integer     NOT NULL DEFAULT 0,
    memory_bytes    bigint      NOT NULL DEFAULT 0,
    mac_addresses   text[]      NOT NULL DEFAULT '{}',
    app_version     text        NOT NULL DEFAULT '',
    -- The first user to log in. NULL until then.
    owner_id        uuid        REFERENCES users (id),
    -- How long a login on this installation keeps working offline.
    offline_access  interval    NOT NULL DEFAULT '7 days',
    created_at      timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    -- Set to block the installation (e.g. stolen computer): no more logins or keys.
    revoked_at      timestamptz,
    CONSTRAINT installations_secret_hash_len CHECK (octet_length(secret_hash) = 32),
    CONSTRAINT installations_machine_id_hash_len CHECK (octet_length(machine_id_hash) = 32),
    CONSTRAINT installations_offline_access_positive CHECK (offline_access > interval '0')
);
CREATE INDEX installations_machine_id_hash_idx ON installations (machine_id_hash);
CREATE INDEX installations_owner_id_idx ON installations (owner_id);

-- Who may log in on which installation. Roles are kept locally, not here.
CREATE TABLE installation_members (
    installation_id uuid        NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- May add and remove members. The owner is always admin.
    is_admin        boolean     NOT NULL DEFAULT false,
    added_by        uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, user_id)
);
CREATE INDEX installation_members_user_id_idx ON installation_members (user_id);

-- Keys of each installation's local database (SQLCipher raw keys).
-- Stored encrypted with the server's KEY_ENCRYPTION_KEY (AES-256-GCM, nonce || ciphertext),
-- so a copy of this database alone does not reveal them.
CREATE TABLE installation_keys (
    installation_id uuid        PRIMARY KEY REFERENCES installations (id) ON DELETE CASCADE,
    -- Version of current_key. 0 = the local database has not been created yet.
    key_version     integer     NOT NULL DEFAULT 0,
    current_key     bytea,
    -- Pending key: becomes current_key (version + 1) after ConfirmKeyRotation.
    next_key        bytea,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT installation_keys_version_non_negative CHECK (key_version >= 0),
    CONSTRAINT installation_keys_current_matches_version CHECK ((key_version = 0) = (current_key IS NULL))
);

CREATE TABLE sessions (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- SHA-256 of the session token; the token itself is only on the client.
    token_hash      bytea       NOT NULL UNIQUE,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    installation_id uuid        NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz NOT NULL,
    revoked_at      timestamptz,
    CONSTRAINT sessions_token_hash_len CHECK (octet_length(token_hash) = 32)
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);

-- +goose Down
DROP TABLE sessions;
DROP TABLE installation_keys;
DROP TABLE installation_members;
DROP TABLE installations;
DROP TABLE users;
