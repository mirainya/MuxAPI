package store

import "time"

// models_routing.go — GORM models for intelligent routing tables.

type RouteDecisionModel struct {
	ID                        int64      `gorm:"primaryKey;autoIncrement"`
	RequestID                 string     `gorm:"column:request_id;type:text;not null;uniqueIndex"`
	GroupID                   int64      `gorm:"column:group_id;type:integer;not null;default:0"`
	Model                     string     `gorm:"type:text;not null;default:''"`
	Protocol                  string     `gorm:"type:text;not null;default:''"`
	Endpoint                  string     `gorm:"type:text;not null;default:''"`
	SessionKey                string     `gorm:"column:session_key;type:text;not null;default:'';index:idx_route_decisions_session_model,priority:1"`
	PrefixHash                string     `gorm:"column:prefix_hash;type:text;not null;default:'';index:idx_route_decisions_prefix_model,priority:1"`
	CacheKey                  string     `gorm:"column:cache_key;type:text;not null;default:''"`
	Strategy                  string     `gorm:"type:text;not null;default:'cost'"`
	Reason                    string     `gorm:"type:text;not null;default:''"`
	SelectedUpstreamID        int64      `gorm:"column:selected_upstream_id;type:integer;not null;default:0"`
	CandidateCount            int        `gorm:"column:candidate_count;type:integer;not null;default:0"`
	ForecastWindowSeconds     int64      `gorm:"column:forecast_window_seconds;type:integer;not null;default:0"`
	ForecastRequests          float64    `gorm:"column:forecast_requests;type:real;not null;default:0"`
	EstimatedInputTokens      int64      `gorm:"column:estimated_input_tokens;type:integer;not null;default:0"`
	ReusablePrefixTokens      int64      `gorm:"column:reusable_prefix_tokens;type:integer;not null;default:0"`
	EstimatedOutputTokens     int64      `gorm:"column:estimated_output_tokens;type:integer;not null;default:0"`
	SelectedCost              *float64   `gorm:"column:selected_cost;type:real"`
	NoCacheCost               *float64   `gorm:"column:no_cache_cost;type:real"`
	CacheCost                 *float64   `gorm:"column:cache_cost;type:real"`
	EstimatedSavings          *float64   `gorm:"column:estimated_savings;type:real"`
	Confidence                float64    `gorm:"type:real;not null;default:0"`
	CacheSelected             bool       `gorm:"column:cache_selected;not null;default:false"`
	Exploration               bool       `gorm:"not null;default:false"`
	ActualCost                *float64   `gorm:"column:actual_cost;type:real"`
	ActualInputTokens         *int64     `gorm:"column:actual_input_tokens;type:integer"`
	ActualOutputTokens        *int64     `gorm:"column:actual_output_tokens;type:integer"`
	ActualCachedTokens        *int64     `gorm:"column:actual_cached_tokens;type:integer"`
	ActualCacheCreationTokens *int64     `gorm:"column:actual_cache_creation_tokens;type:integer"`
	ActualOutcome             string     `gorm:"column:actual_outcome;type:text;not null;default:''"`
	CreatedAt                 time.Time  `gorm:"column:created_at;not null;index:idx_route_decisions_created"`
	CompletedAt               *time.Time `gorm:"column:completed_at"`
}

func (RouteDecisionModel) TableName() string { return "route_decisions" }

type RouteDecisionCandidateModel struct {
	ID                  int64    `gorm:"primaryKey;autoIncrement"`
	DecisionID          int64    `gorm:"column:decision_id;type:integer;not null;index:idx_route_candidates_decision"`
	UpstreamID          int64    `gorm:"column:upstream_id;type:integer;not null;default:0;index:idx_route_candidates_upstream,priority:1"`
	APIKeyHash          string   `gorm:"column:api_key_hash;type:text;not null;default:''"`
	UpstreamName        string   `gorm:"column:upstream_name;type:text;not null;default:''"`
	Protocol            string   `gorm:"type:text;not null;default:''"`
	Priority            int      `gorm:"type:integer;not null;default:0"`
	Eligible            bool     `gorm:"not null;default:true"`
	Selected            bool     `gorm:"not null;default:false"`
	RejectionReason     string   `gorm:"column:rejection_reason;type:text;not null;default:''"`
	PricingSource       string   `gorm:"column:pricing_source;type:text;not null;default:''"`
	PricingConfidence   float64  `gorm:"column:pricing_confidence;type:real;not null;default:0"`
	CacheSupported      bool     `gorm:"column:cache_supported;not null;default:false"`
	CacheExisting       bool     `gorm:"column:cache_existing;not null;default:false"`
	CacheSelected       bool     `gorm:"column:cache_selected;not null;default:false"`
	CacheHitRate        float64  `gorm:"column:cache_hit_rate;type:real;not null;default:0"`
	ForecastTotalCost   *float64 `gorm:"column:forecast_total_cost;type:real"`
	ForecastNoCacheCost *float64 `gorm:"column:forecast_no_cache_cost;type:real"`
	ForecastCacheCost   *float64 `gorm:"column:forecast_cache_cost;type:real"`
	EstimatedSavings    *float64 `gorm:"column:estimated_savings;type:real"`
	BreakEvenRequests   *float64 `gorm:"column:break_even_requests;type:real"`
	ExpectedHits        float64  `gorm:"column:expected_hits;type:real;not null;default:0"`
	ExpectedMisses      float64  `gorm:"column:expected_misses;type:real;not null;default:0"`
	ExpectedCreates     float64  `gorm:"column:expected_creates;type:real;not null;default:0"`
	EstimatedTTFTMs     float64  `gorm:"column:estimated_ttft_ms;type:real;not null;default:0"`
	EstimatedDurationMs float64  `gorm:"column:estimated_duration_ms;type:real;not null;default:0"`
	SuccessRate         float64  `gorm:"column:success_rate;type:real;not null;default:0"`
	DetailsJSON         string   `gorm:"column:details_json;type:text;not null;default:'{}'"`
}

func (RouteDecisionCandidateModel) TableName() string { return "route_decision_candidates" }

type RoutingObservationModel struct {
	ID                  int64      `gorm:"primaryKey;autoIncrement"`
	RequestID           string     `gorm:"column:request_id;type:text;not null;uniqueIndex:idx_routing_obs_req_attempt,priority:1"`
	AttemptNo           int        `gorm:"column:attempt_no;type:integer;not null;default:1;uniqueIndex:idx_routing_obs_req_attempt,priority:2"`
	GroupID             int64      `gorm:"column:group_id;type:integer;not null;default:0"`
	UpstreamID          int64      `gorm:"column:upstream_id;type:integer;not null;default:0;index:idx_routing_observations_upstream_model,priority:1"`
	APIKeyHash          string     `gorm:"column:api_key_hash;type:text;not null;default:''"`
	Model               string     `gorm:"type:text;not null;default:'';index:idx_routing_observations_upstream_model,priority:2"`
	SessionKey          string     `gorm:"column:session_key;type:text;not null;default:'';index:idx_routing_observations_session_model,priority:1"`
	PrefixHash          string     `gorm:"column:prefix_hash;type:text;not null;default:'';index:idx_routing_observations_prefix_model,priority:1"`
	CacheKey            string     `gorm:"column:cache_key;type:text;not null;default:''"`
	PrefixTokens        int64      `gorm:"column:prefix_tokens;type:integer;not null;default:0"`
	InputTokens         int64      `gorm:"column:input_tokens;type:integer;not null;default:0"`
	OutputTokens        int64      `gorm:"column:output_tokens;type:integer;not null;default:0"`
	CachedTokens        int64      `gorm:"column:cached_tokens;type:integer;not null;default:0"`
	CacheCreationTokens int64      `gorm:"column:cache_creation_tokens;type:integer;not null;default:0"`
	TTFTMs              int64      `gorm:"column:ttft_ms;type:integer;not null;default:0"`
	DurationMs          int64      `gorm:"column:duration_ms;type:integer;not null;default:0"`
	Success             bool       `gorm:"not null;default:false"`
	CacheEligible       bool       `gorm:"column:cache_eligible;not null;default:false"`
	CacheHit            bool       `gorm:"column:cache_hit;not null;default:false"`
	CacheCreated        bool       `gorm:"column:cache_created;not null;default:false"`
	CacheExpiresAt      *time.Time `gorm:"column:cache_expires_at"`
	ObservedAt          time.Time  `gorm:"column:observed_at;not null;index:idx_routing_observations_time"`
}

func (RoutingObservationModel) TableName() string { return "routing_observations" }

type UpstreamPrefixCacheStatsModel struct {
	APIKeyHash    string     `gorm:"column:api_key_hash;primaryKey"`
	UpstreamID    int64      `gorm:"column:upstream_id;primaryKey"`
	Model         string     `gorm:"primaryKey"`
	PrefixHash    string     `gorm:"column:prefix_hash;primaryKey"`
	SessionKey    string     `gorm:"column:session_key;type:text;not null;default:''"`
	CacheKey      string     `gorm:"column:cache_key;type:text;not null;default:''"`
	PrefixTokens  int64      `gorm:"column:prefix_tokens;type:integer;not null;default:0"`
	Observations  int64      `gorm:"type:integer;not null;default:0"`
	HitCount      int64      `gorm:"column:hit_count;type:integer;not null;default:0"`
	MissCount     int64      `gorm:"column:miss_count;type:integer;not null;default:0"`
	CreateCount   int64      `gorm:"column:create_count;type:integer;not null;default:0"`
	LastHitAt     *time.Time `gorm:"column:last_hit_at"`
	LastMissAt    *time.Time `gorm:"column:last_miss_at"`
	LastCreatedAt *time.Time `gorm:"column:last_created_at"`
	ExpiresAt     *time.Time `gorm:"column:expires_at;index:idx_upstream_prefix_cache_expiry"`
	FirstSeenAt   time.Time  `gorm:"column:first_seen_at;not null"`
	LastSeenAt    time.Time  `gorm:"column:last_seen_at;not null"`
}

func (UpstreamPrefixCacheStatsModel) TableName() string { return "upstream_prefix_cache_stats" }

type ModelMappingModel struct {
	ID           int64      `gorm:"primaryKey;autoIncrement"`
	UpstreamID   int64      `gorm:"column:upstream_id;type:integer;not null;default:0;uniqueIndex:idx_model_mappings_upstream_source,priority:1"`
	SourceModel  string     `gorm:"column:source_model;type:text;not null;uniqueIndex:idx_model_mappings_upstream_source,priority:2;index:idx_model_mappings_source"`
	TargetModel  string     `gorm:"column:target_model;type:text;not null"`
	MappingType  string     `gorm:"column:mapping_type;type:text;not null;default:'static'"`
	FailureCount int        `gorm:"column:failure_count;type:integer;not null;default:0"`
	ExpiresAt    *time.Time `gorm:"column:expires_at"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;not null"`
}

func (ModelMappingModel) TableName() string { return "model_mappings" }

type UpstreamModelEntry struct {
	UpstreamID int64  `gorm:"column:upstream_id;primaryKey"`
	Model      string `gorm:"primaryKey"`
	UpdatedAt  int64  `gorm:"column:updated_at;type:integer;not null;default:0"`
}

func (UpstreamModelEntry) TableName() string { return "upstream_models" }

// ModelExclusion is one durable negative capability observation for an
// upstream/model pair. A nil ExcludedUntil means the exclusion is permanent.
type ModelExclusion struct {
	UpstreamID    int64      `gorm:"column:upstream_id;primaryKey" json:"upstream_id"`
	Model         string     `gorm:"primaryKey" json:"model"`
	ExcludedUntil *time.Time `gorm:"column:excluded_until" json:"excluded_until,omitempty"`
	FailureCount  int        `gorm:"column:failure_count;not null;default:1" json:"failure_count"`
	LastStatus    int        `gorm:"column:last_status;not null;default:0" json:"last_status"`
	LastReason    string     `gorm:"column:last_reason;type:text;not null;default:''" json:"last_reason"`
	LastFailedAt  time.Time  `gorm:"column:last_failed_at;not null" json:"last_failed_at"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (ModelExclusion) TableName() string { return "upstream_model_exclusions" }
