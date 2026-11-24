-- Membership moves from installations to clinics: whoever may use a clinic's app
-- may log in on every installation of that clinic. The owner of an installation is
-- the owner of its clinic. A clinic admin can create accounts for other people
-- with a temporary password.
--
-- Accounts are unique by national ID, which every new account must have. It stays
-- NULL only for accounts created before that.

-- +goose Up

ALTER TABLE users
    -- Set while the account has a temporary password: no login until it is changed.
    ADD COLUMN must_change_password boolean NOT NULL DEFAULT false,
    -- Who created the account for this person. NULL when they created it themselves.
    ADD COLUMN created_by           uuid REFERENCES users (id) ON DELETE SET NULL;

CREATE TABLE clinic_members (
    clinic_id  uuid        NOT NULL REFERENCES clinics (id) ON DELETE CASCADE,
    user_id    uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- May manage members. The owner is always admin.
    is_admin   boolean     NOT NULL DEFAULT false,
    added_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (clinic_id, user_id)
);
CREATE INDEX clinic_members_user_id_idx ON clinic_members (user_id);

-- Every clinic owner is an admin member of their clinic.
INSERT INTO clinic_members (clinic_id, user_id, is_admin, added_by, created_at)
SELECT id, owner_id, true, owner_id, created_at FROM clinics;

-- Members of an installation become members of its clinic, admin if they were
-- admin on any of the clinic's installations. Installations without a clinic
-- (registered before clinics existed) lose their members: they must be registered again.
INSERT INTO clinic_members (clinic_id, user_id, is_admin, added_by, created_at)
SELECT DISTINCT ON (i.clinic_id, m.user_id) i.clinic_id, m.user_id, m.is_admin, m.added_by, m.created_at
FROM installation_members m
JOIN installations i ON i.id = m.installation_id
WHERE i.clinic_id IS NOT NULL
ORDER BY i.clinic_id, m.user_id, m.is_admin DESC, m.created_at
ON CONFLICT DO NOTHING;

DROP TABLE installation_members;
ALTER TABLE installations DROP COLUMN owner_id;

-- +goose Down
ALTER TABLE installations ADD COLUMN owner_id uuid REFERENCES users (id);
UPDATE installations i SET owner_id = c.owner_id FROM clinics c WHERE c.id = i.clinic_id;
CREATE INDEX installations_owner_id_idx ON installations (owner_id);

CREATE TABLE installation_members (
    installation_id uuid        NOT NULL REFERENCES installations (id) ON DELETE CASCADE,
    user_id         uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    is_admin        boolean     NOT NULL DEFAULT false,
    added_by        uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (installation_id, user_id)
);
CREATE INDEX installation_members_user_id_idx ON installation_members (user_id);
INSERT INTO installation_members (installation_id, user_id, is_admin, added_by, created_at)
SELECT i.id, m.user_id, m.is_admin, m.added_by, m.created_at
FROM clinic_members m
JOIN installations i ON i.clinic_id = m.clinic_id;

DROP TABLE clinic_members;
ALTER TABLE users DROP COLUMN created_by, DROP COLUMN must_change_password;
