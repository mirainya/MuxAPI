-- Reason: expose the calling client and source IP in request audit logs.
-- Scope: additive request metadata columns; existing rows keep empty values.

ALTER TABLE requests ADD COLUMN IF NOT EXISTS client_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN IF NOT EXISTS user_agent TEXT NOT NULL DEFAULT '';
