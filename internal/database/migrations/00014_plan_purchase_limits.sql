-- How many times each owner may buy a plan in the desktop app (Plan.max_purchases_per_owner
-- in proto/dental/v1/subscription.proto), set in the admin panel. 0 means no limit.
-- Selling from the admin panel is not limited.

-- +goose Up
ALTER TABLE plans
    ADD COLUMN max_purchases_per_owner integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT plans_max_purchases_non_negative CHECK (max_purchases_per_owner >= 0);

-- Free plans are trials: once per owner, unless an administrator changes it.
UPDATE plans SET max_purchases_per_owner = 1 WHERE price_rials = 0;

-- Counting an owner's purchases of a plan.
CREATE INDEX subscriptions_user_id_plan_id_idx ON subscriptions (user_id, plan_id);

-- +goose Down
DROP INDEX subscriptions_user_id_plan_id_idx;
ALTER TABLE plans DROP COLUMN max_purchases_per_owner;
