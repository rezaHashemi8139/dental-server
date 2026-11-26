-- Where each clinic is (ClinicInfo.location in proto/dental/v1/installation.proto),
-- in WGS 84 degrees. Required for clinics registered from now on; NULL only for
-- clinics registered before locations existed.

-- +goose Up
ALTER TABLE clinics
    ADD COLUMN latitude  double precision,
    ADD COLUMN longitude double precision,
    ADD CONSTRAINT clinics_location CHECK (
        (latitude IS NULL) = (longitude IS NULL)
        AND latitude BETWEEN -90 AND 90
        AND longitude BETWEEN -180 AND 180);

-- +goose Down
ALTER TABLE clinics DROP COLUMN longitude, DROP COLUMN latitude;
