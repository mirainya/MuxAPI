package store

import (
	"strings"
	"time"

	"github.com/mirainya/muxapi/internal/health"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordModelExclusion creates or refreshes one negative capability record.
// FailureCount is incremented atomically so concurrent gateway failures are
// not lost.
func (s *Store) UpsertModelExclusion(exclusion health.ModelExclusion) error {
	exclusion.Model = strings.TrimSpace(exclusion.Model)
	if exclusion.UpstreamID <= 0 || exclusion.Model == "" {
		return nil
	}
	if exclusion.LastFailedAt.IsZero() {
		exclusion.LastFailedAt = time.Now()
	}
	if exclusion.UpdatedAt.IsZero() {
		exclusion.UpdatedAt = exclusion.LastFailedAt
	}
	return s.gormDB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "upstream_id"}, {Name: "model"}},
		DoUpdates: clause.Assignments(map[string]any{
			"excluded_until": exclusion.ExcludedUntil,
			"failure_count":  gorm.Expr("failure_count + 1"),
			"last_status":    exclusion.LastStatus,
			"last_reason":    exclusion.LastReason,
			"last_failed_at": exclusion.LastFailedAt,
			"updated_at":     exclusion.UpdatedAt,
		}),
	}).Create(&ModelExclusion{
		UpstreamID: exclusion.UpstreamID, Model: exclusion.Model,
		ExcludedUntil: exclusion.ExcludedUntil, FailureCount: max(1, exclusion.FailureCount),
		LastStatus: exclusion.LastStatus, LastReason: exclusion.LastReason,
		LastFailedAt: exclusion.LastFailedAt, UpdatedAt: exclusion.UpdatedAt,
	}).Error
}

// RecordModelExclusion is kept as a small compatibility helper for callers
// outside the health package and for storage-layer tests.
func (s *Store) RecordModelExclusion(upstreamID int64, model string, excludedUntil *time.Time, status int, reason string, failedAt time.Time) error {
	return s.UpsertModelExclusion(health.ModelExclusion{UpstreamID: upstreamID, Model: model,
		ExcludedUntil: excludedUntil, FailureCount: 1, LastStatus: status,
		LastReason: reason, LastFailedAt: failedAt, UpdatedAt: failedAt})
}

// ListModelExclusions returns all durable records, including expired TTL rows.
// Callers decide whether an expired row is eligible for a controlled re-probe.
func (s *Store) ListModelExclusions() ([]ModelExclusion, error) {
	var records []ModelExclusion
	err := s.gormDB.Order("upstream_id, model").Find(&records).Error
	return records, err
}

// LoadModelExclusions implements health.ModelExclusionStore.
func (s *Store) LoadModelExclusions() ([]health.ModelExclusion, error) {
	records, err := s.ListModelExclusions()
	if err != nil {
		return nil, err
	}
	out := make([]health.ModelExclusion, 0, len(records))
	for _, record := range records {
		out = append(out, health.ModelExclusion{
			UpstreamID: record.UpstreamID, Model: record.Model,
			ExcludedUntil: record.ExcludedUntil, FailureCount: record.FailureCount,
			LastStatus: record.LastStatus, LastReason: record.LastReason,
			LastFailedAt: record.LastFailedAt, UpdatedAt: record.UpdatedAt,
		})
	}
	return out, nil
}

// DeleteModelExclusion is the operator-controlled recovery path.
func (s *Store) DeleteModelExclusion(upstreamID int64, model string) error {
	return s.gormDB.Where("upstream_id = ? AND model = ?", upstreamID, strings.TrimSpace(model)).
		Delete(&ModelExclusion{}).Error
}
