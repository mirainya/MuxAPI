-- Reason: distinguish canonical uncached token counts from legacy inclusive rows.
-- Scope: additive audit metadata only; existing token values are unchanged.

ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS input_tokens_normalized BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE request_attempts
    ADD COLUMN IF NOT EXISTS input_tokens_normalized BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE routing_observations
    ADD COLUMN IF NOT EXISTS input_tokens_normalized BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE route_decisions
    ADD COLUMN IF NOT EXISTS actual_input_tokens_normalized BOOLEAN NOT NULL DEFAULT FALSE;
