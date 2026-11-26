-- Clinic owners buy subscriptions in the desktop app (BillingService in
-- proto/dental/v1/billing.proto): they transfer the amount to one of the payment
-- accounts and report it; an administrator approves the payment, which adds the
-- subscription, or rejects it. An online gateway can be added later as another
-- payments.method with its own columns, using the same statuses.
--
-- An owner whose clinic has no access (no subscription in force, or beyond its
-- limits) still logs in, to a billing-only session that can only buy.

-- +goose Up

-- Where owners transfer money to. Managed in the admin panel.
CREATE TABLE payment_accounts (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    bank_name   text        NOT NULL,
    holder_name text        NOT NULL,
    -- At least one of the card number and the IBAN.
    card_number text        NOT NULL DEFAULT '',
    iban        text        NOT NULL DEFAULT '',
    -- Only active accounts are shown to owners, in ascending position.
    active      boolean     NOT NULL DEFAULT true,
    position    integer     NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payment_accounts_card_number CHECK (card_number ~ '^([0-9]{16})?$'),
    CONSTRAINT payment_accounts_iban CHECK (iban ~ '^(IR[0-9]{24})?$'),
    CONSTRAINT payment_accounts_destination CHECK (card_number <> '' OR iban <> '')
);

CREATE TABLE payments (
    id                     uuid        PRIMARY KEY DEFAULT uuidv7(),
    -- The clinic owner who pays.
    user_id                uuid        NOT NULL REFERENCES users (id),
    plan_id                uuid        NOT NULL REFERENCES plans (id),
    -- Copied from the plan when reported, so later changes to the plan do not affect it.
    max_clinics            integer     NOT NULL,
    max_devices_per_clinic integer     NOT NULL,
    duration_days          integer     NOT NULL,
    -- 'extend': starts when the paid time ends; 'upgrade_now': replaces the
    -- subscription in force when approved, its unused value credited.
    mode                   text        NOT NULL,
    -- 'card_to_card' or 'bank_transfer' (manual transfers).
    method                 text        NOT NULL,
    -- 'pending', 'approved', 'rejected' or 'canceled' (by the owner).
    status                 text        NOT NULL DEFAULT 'pending',
    -- Quoted when reported: amount = price - credit.
    price_rials            bigint      NOT NULL,
    credit_rials           bigint      NOT NULL DEFAULT 0,
    amount_rials           bigint      NOT NULL,
    -- Manual transfers: where the money went and how the bank identifies it.
    payment_account_id     uuid        REFERENCES payment_accounts (id),
    reference              text        NOT NULL DEFAULT '',
    paid_at                timestamptz,
    payer_card_last4       text        NOT NULL DEFAULT '',
    -- Set when rejected; shown to the owner.
    rejection_reason       text        NOT NULL DEFAULT '',
    -- Set when approved.
    subscription_id        uuid        REFERENCES subscriptions (id),
    -- Who approved or rejected it; NULL when the owner canceled it.
    reviewed_by            uuid        REFERENCES admins (id) ON DELETE SET NULL,
    reviewed_at            timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT payments_mode CHECK (mode IN ('extend', 'upgrade_now')),
    CONSTRAINT payments_method CHECK (method IN ('card_to_card', 'bank_transfer')),
    CONSTRAINT payments_status CHECK (status IN ('pending', 'approved', 'rejected', 'canceled')),
    CONSTRAINT payments_amounts CHECK (
        price_rials >= 0 AND credit_rials BETWEEN 0 AND price_rials AND amount_rials = price_rials - credit_rials),
    CONSTRAINT payments_approved CHECK ((status = 'approved') = (subscription_id IS NOT NULL))
);
-- An owner has at most one payment waiting for an administrator.
CREATE UNIQUE INDEX payments_one_pending_key ON payments (user_id) WHERE status = 'pending';
CREATE INDEX payments_user_id_idx ON payments (user_id, created_at DESC);
CREATE INDEX payments_status_idx ON payments (status, created_at);

-- A billing-only session may only call BillingService (see SessionInterceptor).
ALTER TABLE sessions ADD COLUMN billing_only boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE sessions DROP COLUMN billing_only;
DROP TABLE payments;
DROP TABLE payment_accounts;
