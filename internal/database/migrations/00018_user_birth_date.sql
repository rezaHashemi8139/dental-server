-- Optional birth date of an account, for birthdays on the clinic app's calendar.
-- Gregorian "YYYY-MM-DD", or '' if unknown, like patients.birth_date. Set by the
-- user (AuthService.UpdateProfile) or by an admin of a clinic they are a member
-- of (MemberService.SetMemberBirthDate). The format is checked by the request rules.

-- +goose Up

ALTER TABLE users ADD COLUMN birth_date text NOT NULL DEFAULT '';

-- +goose Down

ALTER TABLE users DROP COLUMN birth_date;
