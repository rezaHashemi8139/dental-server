-- Subscriptions. Only clinic owners buy them: while one is in force, the members
-- of the owner's clinics may use the app, within its limits on clinics and on
-- installations per clinic. Sold from the admin panel for now.

-- +goose Up

CREATE TABLE plans (
    id                     uuid        PRIMARY KEY DEFAULT uuidv7(),
    name                   text        NOT NULL,
    max_clinics            integer     NOT NULL,
    -- Installations that are not revoked.
    max_devices_per_clinic integer     NOT NULL,
    duration_days          integer     NOT NULL,
    price_rials            bigint      NOT NULL DEFAULT 0,
    -- Inactive plans can no longer be sold; subscriptions sold from them stay as they are.
    active                 boolean     NOT NULL DEFAULT true,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT plans_max_clinics_positive CHECK (max_clinics > 0),
    CONSTRAINT plans_max_devices_positive CHECK (max_devices_per_clinic > 0),
    CONSTRAINT plans_duration_positive CHECK (duration_days > 0),
    CONSTRAINT plans_price_non_negative CHECK (price_rials >= 0)
);

CREATE TABLE subscriptions (
    id                     uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- The clinic owner who bought it.
    user_id                uuid        NOT NULL REFERENCES users (id),
    plan_id                uuid        NOT NULL REFERENCES plans (id),
    -- Copied from the plan when sold, so later changes to the plan do not affect it.
    max_clinics            integer     NOT NULL,
    max_devices_per_clinic integer     NOT NULL,
    price_rials            bigint      NOT NULL,
    -- A user's subscriptions never overlap: a new one starts when the last one ends.
    starts_at              timestamptz NOT NULL,
    ends_at                timestamptz NOT NULL,
    note                   text        NOT NULL DEFAULT '',
    sold_by                uuid        REFERENCES admins (id) ON DELETE SET NULL,
    canceled_at            timestamptz,
    canceled_by            uuid        REFERENCES admins (id) ON DELETE SET NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT subscriptions_max_clinics_positive CHECK (max_clinics > 0),
    CONSTRAINT subscriptions_max_devices_positive CHECK (max_devices_per_clinic > 0),
    CONSTRAINT subscriptions_period CHECK (ends_at > starts_at)
);
CREATE INDEX subscriptions_user_id_ends_at_idx ON subscriptions (user_id, ends_at);

-- +goose Down
DROP TABLE subscriptions;
DROP TABLE plans;
