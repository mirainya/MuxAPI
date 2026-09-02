-- Reason: calculate comparable prompt-cache rates across OpenAI and Anthropic protocols.
-- Scope: additive request audit metadata; existing rows keep neutral defaults.

ALTER TABLE requests
    ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0;

ALTER TABLE request_attempts
    ADD COLUMN IF NOT EXISTS cache_creation_tokens BIGINT NOT NULL DEFAULT 0;
