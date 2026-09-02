-- Reason: route requests through protocol translators when client and upstream schemas differ.
-- Scope: additive upstream protocol selector; existing channels remain passthrough.

ALTER TABLE upstreams
    ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT 'passthrough';
