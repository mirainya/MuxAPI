package store

import "time"

// models_ext.go — continuation of GORM model definitions for larger tables.

type UpstreamBillingStatusModel struct {
	UpstreamID          int64      `gorm:"column:upstream_id;primaryKey"`
	Currency            string     `gorm:"type:text;not null;default:'USD'"`
	Remaining           *float64   `gorm:"type:real"`
	Unlimited           bool       `gorm:"not null;default:false"`
	BillingGroup        string     `gorm:"column:billing_group;type:text;not null;default:''"`
	GroupMultiplier     *float64   `gorm:"column:group_multiplier;type:real"`
	EffectiveMultiplier *float64   `gorm:"column:effective_multiplier;type:real"`
	ReportedListCost    *float64   `gorm:"column:reported_list_cost;type:real"`
	ReportedActualCost  *float64   `gorm:"column:reported_actual_cost;type:real"`
	Status              string     `gorm:"type:text;not null;default:'pending'"`
	ErrorText           string     `gorm:"column:error_text;type:text;not null;default:''"`
	ObservedAt          *time.Time `gorm:"column:observed_at"`
	LastSuccessAt       *time.Time `gorm:"column:last_success_at"`
	RefreshedAt         time.Time  `gorm:"column:refreshed_at;not null"`
}

func (UpstreamBillingStatusModel) TableName() string { return "upstream_billing_status" }

type UpstreamBillingSnapshotModel struct {
	ID                  int64     `gorm:"primaryKey;autoIncrement"`
	UpstreamID          int64     `gorm:"column:upstream_id;type:integer;not null"`
	Currency            string    `gorm:"type:text;not null;default:'USD'"`
	Remaining           *float64  `gorm:"type:real"`
	Unlimited           bool      `gorm:"not null;default:false"`
	BillingGroup        string    `gorm:"column:billing_group;type:text;not null;default:''"`
	GroupMultiplier     *float64  `gorm:"column:group_multiplier;type:real"`
	EffectiveMultiplier *float64  `gorm:"column:effective_multiplier;type:real"`
	ReportedListCost    *float64  `gorm:"column:reported_list_cost;type:real"`
	ReportedActualCost  *float64  `gorm:"column:reported_actual_cost;type:real"`
	ObservedAt          time.Time `gorm:"column:observed_at;not null"`
}

func (UpstreamBillingSnapshotModel) TableName() string { return "upstream_billing_snapshots" }

type ModelPricingModel struct {
	Model                       string   `gorm:"primaryKey;type:text"`
	InputCostPerToken           *float64 `gorm:"column:input_cost_per_token;type:real"`
	OutputCostPerToken          *float64 `gorm:"column:output_cost_per_token;type:real"`
	CacheReadInputTokenCost     *float64 `gorm:"column:cache_read_input_token_cost;type:real"`
	CacheCreationInputTokenCost *float64 `gorm:"column:cache_creation_input_token_cost;type:real"`
}

func (ModelPricingModel) TableName() string { return "model_pricing" }

type PricingCatalogStatusModel struct {
	ID            int64      `gorm:"primaryKey"`
	Source        string     `gorm:"type:text;not null;default:''"`
	Version       string     `gorm:"type:text;not null;default:''"`
	ModelCount    int        `gorm:"column:model_count;type:integer;not null;default:0"`
	LastCheckedAt *time.Time `gorm:"column:last_checked_at"`
	LastSuccessAt *time.Time `gorm:"column:last_success_at"`
	ErrorText     string     `gorm:"column:error_text;type:text;not null;default:''"`
}

func (PricingCatalogStatusModel) TableName() string { return "pricing_catalog_status" }

type ProbeResultModel struct {
	ID        int64     `gorm:"primaryKey;autoIncrement"`
	MonitorID int64     `gorm:"column:monitor_id;type:integer;not null;index:idx_probe_mon_time,priority:1"`
	Status    int       `gorm:"type:integer;not null"`
	LatencyMs int64     `gorm:"column:latency_ms;type:integer;not null"`
	CreatedAt time.Time `gorm:"column:created_at;not null;index:idx_probe_mon_time,priority:2"`
}

func (ProbeResultModel) TableName() string { return "probe_results" }

type RequestModel struct {
	ID                    int64     `gorm:"primaryKey;autoIncrement"`
	RequestID             string    `gorm:"column:request_id;type:text;not null;uniqueIndex"`
	GroupID               int64     `gorm:"column:group_id;type:integer;not null;default:0;index:idx_requests_group_time,priority:1"`
	FinalUpstreamID       int64     `gorm:"column:final_upstream_id;type:integer;not null;default:0"`
	Model                 string    `gorm:"type:text;not null;default:'';index:idx_requests_model_time,priority:1"`
	Endpoint              string    `gorm:"type:text;not null;default:''"`
	KeyName               string    `gorm:"column:key_name;type:text;not null;default:'';index:idx_requests_key_time,priority:1"`
	ClientIP              string    `gorm:"column:client_ip;type:text;not null;default:''"`
	UserAgent             string    `gorm:"column:user_agent;type:text;not null;default:''"`
	Status                int       `gorm:"type:integer;not null;default:0"`
	Outcome               string    `gorm:"type:text;not null;index:idx_requests_outcome_time,priority:1"`
	TTFTMs                int64     `gorm:"column:ttft_ms;type:integer;not null;default:0"`
	DurationMs            int64     `gorm:"column:duration_ms;type:integer;not null;default:0"`
	AttemptCount          int       `gorm:"column:attempt_count;type:integer;not null;default:0"`
	CreatedAt             time.Time `gorm:"column:created_at;not null;index:idx_requests_created_at"`
	CompletedAt           time.Time `gorm:"column:completed_at;not null"`
	ErrorText             string    `gorm:"column:error_text;type:text;not null;default:''"`
	Stream                bool      `gorm:"not null;default:false"`
	RequestBytes          int64     `gorm:"column:request_bytes;type:integer;not null;default:0"`
	ResponseBytes         int64     `gorm:"column:response_bytes;type:integer;not null;default:0"`
	InputTokens           int64     `gorm:"column:input_tokens;type:integer;not null;default:0"`
	InputTokensNormalized bool      `gorm:"column:input_tokens_normalized;not null;default:false"`
	OutputTokens          int64     `gorm:"column:output_tokens;type:integer;not null;default:0"`
	CachedTokens          int64     `gorm:"column:cached_tokens;type:integer;not null;default:0"`
	CacheCreationTokens   int64     `gorm:"column:cache_creation_tokens;type:integer;not null;default:0"`
	StreamCompleted       bool      `gorm:"column:stream_completed;not null;default:false"`
	LastEvent             string    `gorm:"column:last_event;type:text;not null;default:''"`
	UpstreamRequestID     string    `gorm:"column:upstream_request_id;type:text;not null;default:''"`
	ErrorKind             string    `gorm:"column:error_kind;type:text;not null;default:'';index:idx_requests_error_time,priority:1"`
	ErrorSource           string    `gorm:"column:error_source;type:text;not null;default:''"`
}

func (RequestModel) TableName() string { return "requests" }

type RequestAttemptModel struct {
	ID                    int64     `gorm:"primaryKey;autoIncrement"`
	RequestID             string    `gorm:"column:request_id;type:text;not null;uniqueIndex:idx_attempts_request_no,priority:1"`
	AttemptNo             int       `gorm:"column:attempt_no;type:integer;not null;uniqueIndex:idx_attempts_request_no,priority:2"`
	UpstreamID            int64     `gorm:"column:upstream_id;type:integer;not null;default:0;index:idx_attempts_upstream_time,priority:1"`
	Status                int       `gorm:"type:integer;not null;default:0"`
	Outcome               string    `gorm:"type:text;not null"`
	TTFTMs                int64     `gorm:"column:ttft_ms;type:integer;not null;default:0"`
	DurationMs            int64     `gorm:"column:duration_ms;type:integer;not null;default:0"`
	CreatedAt             time.Time `gorm:"column:created_at;not null;index:idx_attempts_upstream_time,priority:2"`
	CompletedAt           time.Time `gorm:"column:completed_at;not null;index:idx_attempts_upstream_completed,priority:2"`
	ErrorText             string    `gorm:"column:error_text;type:text;not null;default:''"`
	Priority              int       `gorm:"type:integer;not null;default:0"`
	SelectionReason       string    `gorm:"column:selection_reason;type:text;not null;default:''"`
	HealthBefore          string    `gorm:"column:health_before;type:text;not null;default:''"`
	HealthAfter           string    `gorm:"column:health_after;type:text;not null;default:''"`
	ResponseBytes         int64     `gorm:"column:response_bytes;type:integer;not null;default:0"`
	Stream                bool      `gorm:"not null;default:false"`
	StreamCompleted       bool      `gorm:"column:stream_completed;not null;default:false"`
	LastEvent             string    `gorm:"column:last_event;type:text;not null;default:''"`
	InputTokens           int64     `gorm:"column:input_tokens;type:integer;not null;default:0"`
	InputTokensNormalized bool      `gorm:"column:input_tokens_normalized;not null;default:false"`
	OutputTokens          int64     `gorm:"column:output_tokens;type:integer;not null;default:0"`
	CachedTokens          int64     `gorm:"column:cached_tokens;type:integer;not null;default:0"`
	CacheCreationTokens   int64     `gorm:"column:cache_creation_tokens;type:integer;not null;default:0"`
	UpstreamRequestID     string    `gorm:"column:upstream_request_id;type:text;not null;default:''"`
	ErrorKind             string    `gorm:"column:error_kind;type:text;not null;default:''"`
	ErrorSource           string    `gorm:"column:error_source;type:text;not null;default:''"`
	Protocol              string    `gorm:"type:text;not null;default:''"`
	MappedModel           string    `gorm:"column:mapped_model;type:text;not null;default:''"`
}

func (RequestAttemptModel) TableName() string { return "request_attempts" }
