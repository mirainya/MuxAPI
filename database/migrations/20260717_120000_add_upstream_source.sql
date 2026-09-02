-- Reason: group a growing upstream pool by provider/source in the admin console.
-- Scope: additive management metadata; routing and existing records are unchanged.

ALTER TABLE upstreams
    ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT '';
