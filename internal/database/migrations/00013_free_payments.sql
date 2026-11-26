-- Purchases with nothing to pay (PAYMENT_METHOD_FREE in proto/dental/v1/subscription.proto):
-- a free plan, or an upgrade whose credit covers the price. They are approved at once,
-- without an administrator (reviewed_by NULL), and have no transfer details.

-- +goose Up
ALTER TABLE payments
    DROP CONSTRAINT payments_method,
    ADD CONSTRAINT payments_method CHECK (method IN ('card_to_card', 'bank_transfer', 'free'));

-- +goose Down
-- Fails while free payments exist.
ALTER TABLE payments
    DROP CONSTRAINT payments_method,
    ADD CONSTRAINT payments_method CHECK (method IN ('card_to_card', 'bank_transfer'));
