-- Add an optional provider multiplier ceiling to each routing group.
-- Known upstream multipliers above the ceiling are excluded before scheduling.
ALTER TABLE groups
    ADD COLUMN max_multiplier DOUBLE PRECISION;

ALTER TABLE groups
    ADD CONSTRAINT groups_max_multiplier_positive
    CHECK (max_multiplier IS NULL OR max_multiplier > 0);
