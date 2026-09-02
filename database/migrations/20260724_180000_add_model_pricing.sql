-- Cache LiteLLM model prices locally so billing audits do not depend on provider-reported list costs.
-- The catalog is replaced transactionally; a failed refresh keeps the last successful version available.
CREATE TABLE model_pricing (
    model TEXT PRIMARY KEY,
    input_cost_per_token DOUBLE PRECISION,
    output_cost_per_token DOUBLE PRECISION,
    cache_read_input_token_cost DOUBLE PRECISION,
    cache_creation_input_token_cost DOUBLE PRECISION
);

CREATE TABLE pricing_catalog_status (
    id SMALLINT PRIMARY KEY,
    source TEXT NOT NULL DEFAULT '',
    version TEXT NOT NULL DEFAULT '',
    model_count INTEGER NOT NULL DEFAULT 0,
    last_checked_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    error_text TEXT NOT NULL DEFAULT '',
    CONSTRAINT pricing_catalog_singleton CHECK (id = 1)
);

CREATE INDEX idx_attempts_upstream_completed
    ON request_attempts (upstream_id, completed_at);
