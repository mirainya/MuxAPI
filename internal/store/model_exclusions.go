package store

import (
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RecordModelExclusion creates or refreshes one negative capability record.
// FailureCount is incremented atomically so concurrent gateway failures are
// not lost.
func (s *Store) RecordModelExclusion(upstreamID int64, model string, excludedUntil *time.Time, status int, reason string, failedAt time.Time) error {
	model = strings.TrimSpace(model)
	if upstreamID <= 0 || model == "" {
		return nil
	}
	if failedAt.IsZero() {
		failedAt = time.Now()
	}
	record := ModelExclusion{
		UpstreamID: upstreamID, Model: model, ExcludedUntil: excludedUntil,
		FailureCount: 1, LastStatus: status, LastReason: reason,
		LastFailedAt: failedAt, UpdatedAt: failedAt,
	}
	return s.gormDB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "upstream_id"}, {Name: "model"}},
		DoUpdates: clause.Assignments(map[string]any{
			"excluded_until": excludedUntil,
			"failure_count":  gorm.Expr("failure_count + 1"),
			"last_status":    status,
			"last_reason":    reason,
			"last_failed_at": failedAt,
			"updated_at":     failedAt,
		}),
	}).Create(&record).Error
}

// ListModelExclusions returns all durable records, including expired TTL rows.
// Callers decide whether an expired row is eligible for a controlled re-probe.
func (s *Store) ListModelExclusions() ([]ModelExclusion, error) {
	var records []ModelExclusion
	err := s.gormDB.Order("upstream_id, model").Find(&records).Error
	return records, err
}

// DeleteModelExclusion is the operator-controlled recovery path.
func (s *Store) DeleteModelExclusion(upstreamID int64, model string) error {
	return s.gormDB.Where("upstream_id = ? AND model = ?", upstreamID, strings.TrimSpace(model)).
		Delete(&ModelExclusion{}).Error
}
