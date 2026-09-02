// Package store 持久化配置、监控和请求审计，并兼容 PostgreSQL 与测试用 SQLite。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glebarez/sqlite"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/mirainya/muxapi/database/migrations"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const requestQueueSize = 4096

type requestWrite struct {
	record  *RequestRecord
	barrier chan struct{}
}

// Store 封装数据库访问；请求审计通过有界队列异步串行写入。
type Store struct {
	gormDB       *gorm.DB
	sqlDB        *sql.DB
	postgres     bool
	requestQueue chan requestWrite
	requestDone  chan struct{}
	requestDrops atomic.Uint64
	closeOnce    sync.Once
}

func newStore(gormDB *gorm.DB, sqlDB *sql.DB, pg bool) *Store {
	s := &Store{
		gormDB:       gormDB,
		sqlDB:        sqlDB,
		postgres:     pg,
		requestQueue: make(chan requestWrite, requestQueueSize),
		requestDone:  make(chan struct{}),
	}
	go s.runRequestWriter()
	return s
}

func (s *Store) timeValue(value time.Time) any {
	if s.postgres {
		return value
	}
	return value.Unix()
}

func (s *Store) unixExpr(column string) string {
	if s.postgres {
		return "CAST(EXTRACT(EPOCH FROM " + column + ") AS BIGINT)"
	}
	return column
}

func (s *Store) hourExpr(column string) string {
	if s.postgres {
		return "CAST(EXTRACT(EPOCH FROM date_trunc('hour', " + column + ")) AS BIGINT)"
	}
	return "(" + column + "/3600)*3600"
}

func (s *Store) bucketExpr(column string, seconds int64) string {
	if s.postgres {
		return fmt.Sprintf("CAST(EXTRACT(EPOCH FROM %s) AS BIGINT)/%d*%d", column, seconds, seconds)
	}
	return fmt.Sprintf("(%s/%d)*%d", column, seconds, seconds)
}

// allModels returns the full list of GORM model structs for AutoMigrate.
func allModels() []any {
	return []any{
		&GroupModel{},
		&UpstreamModel{},
		&TagModel{},
		&UpstreamTagModel{},
		&GroupUpstreamModel{},
		&AccessKeyModel{},
		&MonitorModel{},
		&LogModel{},
		&SettingModel{},
		&UpstreamBillingStatusModel{},
		&UpstreamBillingSnapshotModel{},
		&ModelPricingModel{},
		&PricingCatalogStatusModel{},
		&ProbeResultModel{},
		&RequestModel{},
		&RequestAttemptModel{},
		&RouteDecisionModel{},
		&RouteDecisionCandidateModel{},
		&RoutingObservationModel{},
		&UpstreamPrefixCacheStatsModel{},
		&ModelMappingModel{},
		&UpstreamModelEntry{},
	}
}

// OpenOptions controls startup behavior for an existing database.
type OpenOptions struct {
	ReadOnly bool
}

// Open 根据连接串选择数据库。PostgreSQL 使用版本化迁移，SQLite 使用
// GORM AutoMigrate 创建测试和本地开发所需的 schema。
func Open(databaseURL string) (*Store, error) {
	return OpenWithOptions(databaseURL, OpenOptions{})
}

// OpenWithOptions opens a store with configurable startup behavior.
func OpenWithOptions(databaseURL string, options OpenOptions) (*Store, error) {
	if strings.HasPrefix(databaseURL, "postgres://") || strings.HasPrefix(databaseURL, "postgresql://") {
		return openPostgres(databaseURL, options)
	}
	if databaseURL == "" {
		return nil, errors.New("MUXAPI_DATABASE_URL is required")
	}
	return openSQLite(databaseURL)
}

func openPostgres(databaseURL string, options OpenOptions) (*Store, error) {
	gormDB, err := gorm.Open(postgres.New(postgres.Config{
		DSN:                  databaseURL,
		PreferSimpleProtocol: true,
	}), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(10)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	if !options.ReadOnly {
		if err := runPostgresMigrations(ctx, sqlDB); err != nil {
			sqlDB.Close()
			return nil, fmt.Errorf("migrate PostgreSQL: %w", err)
		}
	} else {
		slog.Info("skipping PostgreSQL migrations in read-only mode")
	}
	return newStore(gormDB, sqlDB, true), nil
}

// runPostgresMigrations applies each embedded migration once, in filename order.
func runPostgresMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	// Older dev builds used GORM AutoMigrate before versioned migrations were
	// restored. Such a database has the complete intelligent-routing schema but
	// an empty schema_migrations table. Mark only the historical migrations as
	// applied, then let the current and future migrations run normally.
	var appliedCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&appliedCount); err != nil {
		return err
	}
	if appliedCount == 0 {
		var hasRoutingTables, hasCacheMode bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema=current_schema() AND table_name='route_decisions')`).Scan(&hasRoutingTables); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name='upstreams' AND column_name='cache_mode')`).Scan(&hasCacheMode); err != nil {
			return err
		}
		if hasRoutingTables && hasCacheMode {
			const currentMigration = "20260902_100000_add_intelligent_routing.sql"
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= currentMigration {
					continue
				}
				version := strings.TrimSuffix(entry.Name(), ".sql")
				if _, err := db.ExecContext(ctx,
					`INSERT INTO schema_migrations(version) VALUES($1) ON CONFLICT DO NOTHING`, version); err != nil {
					return fmt.Errorf("baseline legacy GORM migration %s: %w", entry.Name(), err)
				}
			}
		}
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var applied bool
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, version).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.Files.ReadFile(entry.Name())
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, version)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func openSQLite(path string) (*Store, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	dsn := path + sep + "_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	gormDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	sqlDB, err := gormDB.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	if err := gormDB.AutoMigrate(allModels()...); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate SQLite: %w", err)
	}
	createCustomIndexes(gormDB, false)
	runSQLiteDataMigrations(sqlDB)
	return newStore(gormDB, sqlDB, false), nil
}

// createCustomIndexes creates indexes that GORM AutoMigrate cannot express
// (partial indexes, multi-column indexes with specific ordering, etc.).
func createCustomIndexes(db *gorm.DB, pg bool) {
	indexes := []string{
		`CREATE INDEX IF NOT EXISTS idx_logs_group_time ON logs(group_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_group_time ON requests(group_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_model_time ON requests(model, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_outcome_time ON requests(outcome, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_key_time ON requests(key_name, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_requests_error_time ON requests(error_kind, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_attempts_request ON request_attempts(request_id, attempt_no)`,
		`CREATE INDEX IF NOT EXISTS idx_attempts_upstream_time ON request_attempts(upstream_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_attempts_upstream_completed ON request_attempts(upstream_id, completed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_upstream_billing_snapshots_time ON upstream_billing_snapshots(upstream_id, observed_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_route_decisions_created ON route_decisions(created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_route_decisions_session_model ON route_decisions(session_key, model, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_route_decisions_prefix_model ON route_decisions(prefix_hash, model, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_route_candidates_upstream ON route_decision_candidates(upstream_id, decision_id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_routing_observations_time ON routing_observations(observed_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_routing_observations_session_model ON routing_observations(session_key, model, observed_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_routing_observations_prefix_model ON routing_observations(prefix_hash, model, observed_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_routing_observations_upstream_model ON routing_observations(upstream_id, model, observed_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_upstream_prefix_cache_expiry ON upstream_prefix_cache_stats(api_key_hash, upstream_id, model, expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_model_mappings_source ON model_mappings(source_model)`,
		`CREATE INDEX IF NOT EXISTS idx_model_mappings_upstream ON model_mappings(upstream_id, source_model)`,
	}
	if !pg {
		indexes = append(indexes,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_tags_name_ci ON tags(LOWER(name))`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_primary_tag ON upstream_tags(upstream_id) WHERE is_primary=1`,
			`CREATE INDEX IF NOT EXISTS idx_upstream_tags_tag ON upstream_tags(tag_id, upstream_id)`,
		)
	} else {
		indexes = append(indexes,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_tags_name_ci ON tags(LOWER(name))`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_upstream_primary_tag ON upstream_tags(upstream_id) WHERE is_primary=true`,
			`CREATE INDEX IF NOT EXISTS idx_upstream_tags_tag ON upstream_tags(tag_id, upstream_id)`,
		)
	}
	for _, ddl := range indexes {
		if err := db.Exec(ddl).Error; err != nil {
			slog.Debug("create index (may already exist)", "err", err)
		}
	}
}

// runSQLiteDataMigrations applies one-time data fixups for SQLite (test/dev).
func runSQLiteDataMigrations(db *sql.DB) {
	// Migrate source column to tags for legacy data
	db.Exec(`INSERT OR IGNORE INTO tags(name,color,sort_order)
		SELECT DISTINCT TRIM(source),'gray',0 FROM upstreams WHERE TRIM(source)<>''`)
	db.Exec(`INSERT OR IGNORE INTO upstream_tags(upstream_id,tag_id,is_primary)
		SELECT u.id,t.id,1 FROM upstreams u JOIN tags t ON LOWER(t.name)=LOWER(TRIM(u.source)) WHERE TRIM(u.source)<>''`)
	// Intelligent routing default retention (permanent)
	var retentionMigration string
	_ = db.QueryRow(`SELECT value FROM settings WHERE key=?`, "intelligent_routing_retention_migrated").Scan(&retentionMigration)
	if retentionMigration != "1" {
		db.Exec(`INSERT INTO settings(key,value) VALUES('request_retention_days','0')
			ON CONFLICT(key) DO UPDATE SET value='0'`)
		db.Exec(`INSERT INTO settings(key,value) VALUES('billing_snapshot_retention_days','0')
			ON CONFLICT(key) DO UPDATE SET value='0'`)
		db.Exec(`INSERT INTO settings(key,value) VALUES('probe_retention_hours','0')
			ON CONFLICT(key) DO UPDATE SET value='0'`)
		db.Exec(`INSERT INTO settings(key,value) VALUES('intelligent_routing_retention_migrated','1')
			ON CONFLICT(key) DO UPDATE SET value='1'`)
	}
}

// --- Raw SQL helpers (used by complex queries that remain hand-written) ---

// bindPostgres 只替换 SQL 字符串字面量之外的问号占位符。
func bindPostgres(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	arg := 1
	inString := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			b.WriteByte(ch)
			if inString && i+1 < len(query) && query[i+1] == '\'' {
				b.WriteByte(query[i+1])
				i++
				continue
			}
			inString = !inString
			continue
		}
		if ch == '?' && !inString {
			fmt.Fprintf(&b, "$%d", arg)
			arg++
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}

func (s *Store) bind(query string) string {
	if s.postgres {
		return bindPostgres(query)
	}
	return query
}

func (s *Store) exec(query string, args ...any) (sql.Result, error) {
	return s.sqlDB.Exec(s.bind(query), args...)
}

func (s *Store) query(query string, args ...any) (*sql.Rows, error) {
	return s.sqlDB.Query(s.bind(query), args...)
}

func (s *Store) queryRow(query string, args ...any) *sql.Row {
	return s.sqlDB.QueryRow(s.bind(query), args...)
}

type rawTx struct {
	*sql.Tx
	postgres bool
}

func (s *Store) beginTx() (*rawTx, error) {
	tx, err := s.sqlDB.Begin()
	if err != nil {
		return nil, err
	}
	return &rawTx{Tx: tx, postgres: s.postgres}, nil
}

func (t *rawTx) Exec(query string, args ...any) (sql.Result, error) {
	if t.postgres {
		query = bindPostgres(query)
	}
	return t.Tx.Exec(query, args...)
}

func (t *rawTx) QueryRow(query string, args ...any) *sql.Row {
	if t.postgres {
		query = bindPostgres(query)
	}
	return t.Tx.QueryRow(query, args...)
}
