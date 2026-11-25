-- Admin panel sessions are proven by a JWT in an HttpOnly cookie plus a CSRF token
-- sent in a header (see AdminService in proto/dental/v1/admin.proto). The JWT names
-- its admin_sessions row, which is still checked on every call so that logging out
-- or disabling an administrator ends the session at once. The opaque session token
-- is gone: sessions from before this change end and administrators log in again.

-- +goose Up
DELETE FROM admin_sessions;
ALTER TABLE admin_sessions
    DROP COLUMN token_hash,
    -- SHA-256 of the session's CSRF token; the token itself is only on the client.
    ADD COLUMN csrf_hash bytea NOT NULL,
    ADD CONSTRAINT admin_sessions_csrf_hash_len CHECK (octet_length(csrf_hash) = 32);

-- +goose Down
DELETE FROM admin_sessions;
ALTER TABLE admin_sessions
    DROP COLUMN csrf_hash,
    ADD COLUMN token_hash bytea NOT NULL UNIQUE,
    ADD CONSTRAINT admin_sessions_token_hash_len CHECK (octet_length(token_hash) = 32);
