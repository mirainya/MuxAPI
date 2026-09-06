CREATE TABLE IF NOT EXISTS upstream_model_exclusions (
    upstream_id BIGINT NOT NULL REFERENCES upstreams(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    excluded_until TIMESTAMPTZ,
    failure_count INTEGER NOT NULL DEFAULT 1,
    last_status INTEGER NOT NULL DEFAULT 0,
    last_reason TEXT NOT NULL DEFAULT '',
    last_failed_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (upstream_id, model)
);

CREATE INDEX IF NOT EXISTS idx_upstream_model_exclusions_updated
    ON upstream_model_exclusions(updated_at DESC);
