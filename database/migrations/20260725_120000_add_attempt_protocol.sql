-- Reason: snapshot the protocol used by each attempt for historical cost accounting.
-- Scope: additive request-attempt metadata; old rows retain an empty value.
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT '';
