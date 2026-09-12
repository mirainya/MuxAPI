package store

import (
	"context"
	"log/slog"
	"time"
)

// --- 请求审计 ---

type RequestAttemptRecord struct {
	AttemptNo             int
	Protocol              string // 快照当次协议，供费用比对使用正确的 cached_tokens 口径
	MappedModel           string
	UpstreamID            int64
	Priority              int
	SelectionReason       string
	HealthBefore          string
	HealthAfter           string
	Status                int
	Outcome               string
	TTFTMs                int64
	DurationMs            int64
	ResponseBytes         int64
	Stream                bool
	StreamCompleted       bool
	LastEvent             string
	InputTokens           int64
	InputTokensNormalized bool
	OutputTokens          int64
	CachedTokens          int64
	CacheCreationTokens   int64
	UpstreamRequestID     string
	ErrorKind             string
	ErrorSource           string
	CreatedAt             time.Time
	CompletedAt           time.Time
	Error                 string
}

type RequestRecord struct {
	RequestID             string
	GroupID               int64
	FinalUpstreamID       int64
	Model                 string
	Endpoint              string
	KeyName               string
	ClientIP              string
	UserAgent             string
	Stream                bool
	RequestBytes          int64
	ResponseBytes         int64
	InputTokens           int64
	InputTokensNormalized bool `json:"-"`
	OutputTokens          int64
	CachedTokens          int64
	CacheCreationTokens   int64
	StreamCompleted       bool
	LastEvent             string
	UpstreamRequestID     string
	ErrorKind             string
	ErrorSource           string
	Status                int
	Outcome               string
	TTFTMs                int64
	DurationMs            int64
	CreatedAt             time.Time
	CompletedAt           time.Time
	Error                 string
	Attempts              []RequestAttemptRecord
}

type RequestAttemptEntry struct {
	ID                    int64   `json:"id"`
	AttemptNo             int     `json:"attempt_no"`
	UpstreamID            int64   `json:"upstream_id"`
	UpstreamName          string  `json:"upstream_name"`
	MappedModel           string  `json:"mapped_model,omitempty"`
	Priority              int     `json:"priority"`
	SelectionReason       string  `json:"selection_reason"`
	HealthBefore          string  `json:"health_before"`
	HealthAfter           string  `json:"health_after"`
	Status                int     `json:"status"`
	Outcome               string  `json:"outcome"`
	TTFTMs                int64   `json:"ttft_ms"`
	DurationMs            int64   `json:"duration_ms"`
	ResponseBytes         int64   `json:"response_bytes"`
	Stream                bool    `json:"stream"`
	StreamCompleted       bool    `json:"stream_completed"`
	LastEvent             string  `json:"last_event"`
	InputTokens           int64   `json:"input_tokens"`
	InputTokensNormalized bool    `json:"-"`
	OutputTokens          int64   `json:"output_tokens"`
	CachedTokens          int64   `json:"cached_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheInputTokens      int64   `json:"cache_input_tokens"`
	CacheRate             float64 `json:"cache_rate"`
	UpstreamRequestID     string  `json:"upstream_request_id"`
	ErrorKind             string  `json:"error_kind"`
	ErrorSource           string  `json:"error_source"`
	CreatedAt             int64   `json:"created_at"`
	CompletedAt           int64   `json:"completed_at"`
	Error                 string  `json:"error"`
}

type RequestRouteStep struct {
	AttemptNo    int    `json:"attempt_no"`
	UpstreamID   int64  `json:"upstream_id"`
	UpstreamName string `json:"upstream_name"`
	Status       int    `json:"status"`
	Outcome      string `json:"outcome"`
	ErrorKind    string `json:"error_kind"`
}

type RequestEntry struct {
	ID                    int64   `json:"id"`
	RequestID             string  `json:"request_id"`
	GroupID               int64   `json:"group_id"`
	GroupName             string  `json:"group_name"`
	FinalUpstreamID       int64   `json:"final_upstream_id"`
	FinalUpstreamName     string  `json:"final_upstream_name"`
	Model                 string  `json:"model"`
	Endpoint              string  `json:"endpoint"`
	KeyName               string  `json:"key_name"`
	ClientIP              string  `json:"client_ip"`
	UserAgent             string  `json:"user_agent"`
	Stream                bool    `json:"stream"`
	RequestBytes          int64   `json:"request_bytes"`
	ResponseBytes         int64   `json:"response_bytes"`
	InputTokens           int64   `json:"input_tokens"`
	InputTokensNormalized bool    `json:"-"`
	OutputTokens          int64   `json:"output_tokens"`
	CachedTokens          int64   `json:"cached_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheInputTokens      int64   `json:"cache_input_tokens"`
	CacheRate             float64 `json:"cache_rate"`
	// EstimatedInputTokens comes from the routing decision captured for this
	// request. TokenInflation is actual provider prompt context divided by that
	// client-side estimate; zero means no comparable sample is available.
	EstimatedInputTokens int64                  `json:"estimated_input_tokens"`
	TokenInflation       float64                `json:"token_inflation"`
	StreamCompleted      bool                   `json:"stream_completed"`
	LastEvent            string                 `json:"last_event"`
	UpstreamRequestID    string                 `json:"upstream_request_id"`
	ErrorKind            string                 `json:"error_kind"`
	ErrorSource          string                 `json:"error_source"`
	Status               int                    `json:"status"`
	Outcome              string                 `json:"outcome"`
	TTFTMs               int64                  `json:"ttft_ms"`
	DurationMs           int64                  `json:"duration_ms"`
	AttemptCount         int                    `json:"attempt_count"`
	CreatedAt            int64                  `json:"created_at"`
	CompletedAt          int64                  `json:"completed_at"`
	Error                string                 `json:"error"`
	Route                []RequestRouteStep     `json:"route,omitempty"`
	Attempts             []*RequestAttemptEntry `json:"attempts,omitempty"`
}

type RequestPage struct {
	Entries    []*RequestEntry `json:"entries"`
	HasMore    bool            `json:"has_more"`
	NextCursor int64           `json:"next_cursor"`
}

// EnqueueRequest 非阻塞提交审计记录；队列已满时返回 false 并累计丢弃数。
func (s *Store) EnqueueRequest(record RequestRecord) bool {
	select {
	case s.requestQueue <- requestWrite{record: &record}:
		return true
	default:
		dropped := s.requestDrops.Add(1)
		if dropped == 1 || dropped%100 == 0 {
			slog.Error("request audit queue full", "dropped", dropped)
		}
		return false
	}
}

// FlushRequests 等待调用前已入队的审计记录写完，常用于测试和关闭流程。
func (s *Store) FlushRequests(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case s.requestQueue <- requestWrite{barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RequestDrops 返回因队列满或数据库写入失败而丢失的审计数。
func (s *Store) RequestDrops() uint64 { return s.requestDrops.Load() }

// runRequestWriter 单协程消费队列，保证屏障与先前审计记录的顺序。
func (s *Store) runRequestWriter() {
	defer close(s.requestDone)
	for item := range s.requestQueue {
		if item.barrier != nil {
			close(item.barrier)
			continue
		}
		if err := s.writeRequest(*item.record); err != nil {
			s.requestDrops.Add(1)
			slog.Error("write request audit failed", "request_id", item.record.RequestID, "err", err)
		}
	}
}

func (s *Store) writeRequest(record RequestRecord) error {
	tx, err := s.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO requests(
		request_id,group_id,final_upstream_id,model,endpoint,key_name,client_ip,user_agent,status,outcome,
		ttft_ms,duration_ms,attempt_count,created_at,completed_at,error_text,stream,
		request_bytes,response_bytes,input_tokens,input_tokens_normalized,output_tokens,cached_tokens,cache_creation_tokens,stream_completed,
		last_event,upstream_request_id,error_kind,error_source)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.RequestID, record.GroupID, record.FinalUpstreamID, record.Model, record.Endpoint,
		record.KeyName, record.ClientIP, record.UserAgent, record.Status, record.Outcome, record.TTFTMs, record.DurationMs,
		len(record.Attempts), s.timeValue(record.CreatedAt), s.timeValue(record.CompletedAt), record.Error,
		record.Stream, record.RequestBytes, record.ResponseBytes, record.InputTokens, record.InputTokensNormalized, record.OutputTokens,
		record.CachedTokens, record.CacheCreationTokens, record.StreamCompleted, record.LastEvent, record.UpstreamRequestID,
		record.ErrorKind, record.ErrorSource)
	if err != nil {
		return err
	}
	for _, attempt := range record.Attempts {
		_, err = tx.Exec(`INSERT INTO request_attempts(
			request_id,attempt_no,upstream_id,status,outcome,ttft_ms,duration_ms,created_at,completed_at,error_text,
			priority,selection_reason,health_before,health_after,response_bytes,stream,stream_completed,last_event,
			input_tokens,input_tokens_normalized,output_tokens,cached_tokens,cache_creation_tokens,upstream_request_id,error_kind,error_source,
			protocol,mapped_model)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			record.RequestID, attempt.AttemptNo, attempt.UpstreamID, attempt.Status, attempt.Outcome,
			attempt.TTFTMs, attempt.DurationMs, s.timeValue(attempt.CreatedAt),
			s.timeValue(attempt.CompletedAt), attempt.Error, attempt.Priority, attempt.SelectionReason,
			attempt.HealthBefore, attempt.HealthAfter, attempt.ResponseBytes, attempt.Stream,
			attempt.StreamCompleted, attempt.LastEvent, attempt.InputTokens, attempt.InputTokensNormalized, attempt.OutputTokens,
			attempt.CachedTokens, attempt.CacheCreationTokens, attempt.UpstreamRequestID, attempt.ErrorKind, attempt.ErrorSource,
			attempt.Protocol, attempt.MappedModel)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
