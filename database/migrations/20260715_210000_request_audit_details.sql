-- Reason: make request records sufficient for routing and streaming incident analysis.
-- Scope: additive audit columns and indexes; existing rows keep neutral defaults.

ALTER TABLE requests ADD COLUMN IF NOT EXISTS stream BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS request_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS response_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS input_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS output_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS cached_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS stream_completed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE requests ADD COLUMN IF NOT EXISTS last_event TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN IF NOT EXISTS upstream_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN IF NOT EXISTS error_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE requests ADD COLUMN IF NOT EXISTS error_source TEXT NOT NULL DEFAULT '';

ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS priority INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS selection_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS health_before TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS health_after TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS response_bytes BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS stream BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS stream_completed BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS last_event TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS input_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS output_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS cached_tokens BIGINT NOT NULL DEFAULT 0;
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS upstream_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS error_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE request_attempts ADD COLUMN IF NOT EXISTS error_source TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_requests_key_time ON requests(key_name, created_at);
CREATE INDEX IF NOT EXISTS idx_requests_error_time ON requests(error_kind, created_at);
