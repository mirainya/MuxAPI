-- Reason: persist the user-defined order of groups in the admin console.
-- Scope: additive management metadata; routing and existing group data are unchanged.

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS sort_order INTEGER NOT NULL DEFAULT 0;
