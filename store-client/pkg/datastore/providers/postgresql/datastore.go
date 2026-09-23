// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package postgresql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/lib/pq" // also registers the PostgreSQL driver
	"github.com/lib/pq/pqerror"
	"github.com/prometheus/client_golang/prometheus"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
	"github.com/nvidia/nvsentinel/store-client/pkg/utils"
)

// PostgreSQLDataStore implements the DataStore interface for PostgreSQL
type PostgreSQLDataStore struct {
	db                    *sql.DB
	connString            string // Connection string for creating LISTEN connections
	maintenanceEventStore datastore.MaintenanceEventStore
	healthEventStore      datastore.HealthEventStore
	runtimeUpgradeCancel  context.CancelFunc
	runtimeUpgradeDone    <-chan struct{}
	// metricsRegisterer is where change stream metrics are registered; nil means the default
	// Prometheus registry.
	metricsRegisterer prometheus.Registerer
}

// NewPostgreSQLStore creates a new PostgreSQL datastore
func NewPostgreSQLStore(ctx context.Context, config datastore.DataStoreConfig) (datastore.DataStore, error) {
	// Validate configuration
	if config.Connection.Host == "" {
		return nil, fmt.Errorf("host is required")
	}

	if config.Connection.Database == "" {
		return nil, fmt.Errorf("database is required")
	}

	if config.Connection.Username == "" {
		return nil, fmt.Errorf("username is required")
	}

	if config.Connection.Port < 1 || config.Connection.Port > 65535 {
		return nil, fmt.Errorf("port must be between 1 and 65535")
	}

	connectionString := buildConnectionString(config.Connection)

	db, err := otelsql.Open("postgres", connectionString,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithTracerProvider(tracing.GetChildOnlyTracerProvider()),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to open PostgreSQL connection: %w", err)
	}

	// Test connection
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping PostgreSQL database: %w", err)
	}

	// Create tables if they don't exist, one component at a time. This runs
	// before the pool limit is applied: the setup lock holds one connection
	// while the setup statements use another, which a pool limited to a
	// single connection could never hand out.
	if err := withSetupLock(ctx, db, func() error { return createTables(ctx, db) }); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	// Set connection pool settings
	ConfigureConnectionPool(db, config.Options)

	if _, err := otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
	); err != nil {
		slog.Warn("Failed to register DB stats metrics", "error", err)
	}

	store := &PostgreSQLDataStore{
		db:                db,
		connString:        connectionString, // Store for LISTEN connections
		metricsRegisterer: config.MetricsRegisterer,
	}
	store.maintenanceEventStore = NewPostgreSQLMaintenanceEventStore(db)
	store.healthEventStore = NewPostgreSQLHealthEventStore(db)
	store.startRuntimeUpgrades()

	slog.Info("Successfully connected to PostgreSQL database", "host", config.Connection.Host)

	return store, nil
}

// defaultMaxOpenConns preserves the previously hardcoded connection pool limit.
const defaultMaxOpenConns = 25

// defaultMaxIdleConns and defaultConnMaxLifetime are the shared idle-connection
// and connection-lifetime defaults applied to every PostgreSQL pool.
const (
	defaultMaxIdleConns    = 10
	defaultConnMaxLifetime = time.Hour
)

// ConfigureConnectionPool applies the shared PostgreSQL connection pool
// settings to db: the max open connections resolved by resolveMaxOpenConns
// plus the default idle-connection count and connection lifetime. It is used
// by NewPostgreSQLStore and by the client factory so both paths honor the
// same pool configuration.
func ConfigureConnectionPool(db *sql.DB, options map[string]string) {
	db.SetMaxOpenConns(resolveMaxOpenConns(options))
	db.SetMaxIdleConns(defaultMaxIdleConns)
	db.SetConnMaxLifetime(defaultConnMaxLifetime)
}

// resolveMaxOpenConns returns the connection pool limit: the shared
// datastore.MaxConnections resolution (the maxConnections option, then the
// DATASTORE_MAX_CONNECTIONS environment variable) or the default of 25.
func resolveMaxOpenConns(options map[string]string) int {
	if size := datastore.MaxConnections(options); size > 0 {
		return size
	}

	return defaultMaxOpenConns
}

// MaintenanceEventStore returns the maintenance event store
func (p *PostgreSQLDataStore) MaintenanceEventStore() datastore.MaintenanceEventStore {
	return p.maintenanceEventStore
}

// HealthEventStore returns the health event store
func (p *PostgreSQLDataStore) HealthEventStore() datastore.HealthEventStore {
	return p.healthEventStore
}

// Ping tests the database connection
func (p *PostgreSQLDataStore) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}

// Close closes the database connection
func (p *PostgreSQLDataStore) Close(ctx context.Context) error {
	if p.runtimeUpgradeCancel != nil {
		p.runtimeUpgradeCancel()
	}

	if p.runtimeUpgradeDone != nil {
		select {
		case <-p.runtimeUpgradeDone:
		case <-ctx.Done():
			return errors.Join(ctx.Err(), p.db.Close())
		}
	}

	return p.db.Close()
}

// Provider returns the provider type
func (p *PostgreSQLDataStore) Provider() datastore.DataStoreProvider {
	return datastore.ProviderPostgreSQL
}

// GetDB returns the underlying database connection for change stream watchers
func (p *PostgreSQLDataStore) GetDB() *sql.DB {
	return p.db
}

// NewChangeStreamWatcher creates a new change stream watcher for the PostgreSQL datastore
// This method makes PostgreSQL compatible with the datastore abstraction layer
func (p *PostgreSQLDataStore) NewChangeStreamWatcher(
	ctx context.Context, config any,
) (datastore.ChangeStreamWatcher, error) {
	clientName, tableName, pipeline, err := parseWatcherConfig(config)
	if err != nil {
		return nil, err
	}

	resumeControlDecision, err := client.ResetResumeTokenOnStartIfConfigured(
		ctx,
		p.GetDatabaseClient(),
		client.TokenConfig{ClientName: clientName},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to reset change stream resume token on startup: %w", err)
	}

	pipelineFilter := buildPipelineFilter(pipeline, tableName, clientName)
	pipeline, _ = client.ResolvePipelineOptions(pipeline)

	// Convert PascalCase table name to snake_case for PostgreSQL compatibility
	snakeCaseTableName := toSnakeCase(tableName)

	slog.Info("Creating PostgreSQL changestream watcher",
		"originalTableName", tableName,
		"postgresTableName", snakeCaseTableName,
		"clientName", clientName)

	// Create and return PostgreSQL change stream watcher
	// Default to hybrid mode for best performance and reliability
	watcher := NewPostgreSQLChangeStreamWatcher(p.db, clientName, snakeCaseTableName, p.connString, ModeHybrid)
	watcher.pipeline = pipeline
	watcher.pipelineFilter = pipelineFilter

	client.RegisterChangeStreamLag(p.metricsRegisterer, clientName, watcher)

	// Wrap the watcher to provide Unwrap() support for backward compatibility
	return NewPostgreSQLChangeStreamWatcherWithUnwrap(watcher, resumeControlDecision), nil
}

func parseWatcherConfig(config any) (string, string, any, error) {
	configMap, ok := config.(map[string]any)
	if !ok {
		return "", "", nil, fmt.Errorf("unsupported config type: %T", config)
	}

	var clientName, tableName string

	if val, ok := configMap["ClientName"].(string); ok {
		clientName = val
	}

	if val, ok := configMap["TableName"].(string); ok {
		tableName = val
	}

	// Also support MongoDB-style CollectionName for compatibility
	if val, ok := configMap["CollectionName"].(string); ok {
		tableName = val
	}

	if clientName == "" {
		return "", "", nil, fmt.Errorf("ClientName is required")
	}

	if tableName == "" {
		return "", "", nil, fmt.Errorf("TableName (or CollectionName) is required")
	}

	return clientName, tableName, configMap["Pipeline"], nil
}

// buildPipelineFilter creates a two-layer pipeline filter for optimal performance:
// 1. Server-side: SQL WHERE clause (built from raw pipeline in fetchNewChanges)
// 2. Application-side: PipelineFilter (handles edge cases SQL can't express)
func buildPipelineFilter(pipeline any, tableName, clientName string) *PipelineFilter {
	if pipeline == nil {
		return nil
	}

	filter, err := NewPipelineFilter(pipeline)
	if err != nil {
		slog.Warn("Failed to parse MongoDB pipeline for PostgreSQL filtering",
			"error", err,
			"tableName", tableName,
			"clientName", clientName,
			"action", "all events will be returned without filtering")

		return nil
	}

	if filter != nil {
		slog.Info("PostgreSQL change stream will filter events using parsed MongoDB pipeline",
			"tableName", tableName,
			"clientName", clientName,
			"stages", len(filter.stages))
	}

	return filter
}

// --- Backward Compatibility Methods for MongoDB-style Type Assertions ---

// GetDatabaseClient returns a PostgreSQL implementation of client.DatabaseClient
// This method exists for compatibility with services that type-assert for MongoDB-style operations
func (p *PostgreSQLDataStore) GetDatabaseClient() client.DatabaseClient {
	return NewPostgreSQLDatabaseClientWithConnString(p.db, "health_events", p.connString)
}

// CreateChangeStreamWatcher creates a change stream watcher for PostgreSQL
// This method exists for compatibility with services that use MongoDB-style type assertions
// It delegates to NewChangeStreamWatcher with the appropriate configuration
func (p *PostgreSQLDataStore) CreateChangeStreamWatcher(
	ctx context.Context, clientName string, pipeline any,
) (datastore.ChangeStreamWatcher, error) {
	config := map[string]any{
		"ClientName": clientName,
		"TableName":  "health_events", // Default table name
		"Pipeline":   pipeline,
	}

	return p.NewChangeStreamWatcher(ctx, config)
}

// Verify that PostgreSQLDataStore implements the DataStore interface
var _ datastore.DataStore = (*PostgreSQLDataStore)(nil)

// buildConnectionString creates a PostgreSQL connection string
func buildConnectionString(conn datastore.ConnectionConfig) string {
	params := make([]string, 0)

	params = append(params, fmt.Sprintf("host=%s", conn.Host))

	if conn.Port > 0 {
		params = append(params, fmt.Sprintf("port=%d", conn.Port))
	}

	if conn.Database != "" {
		params = append(params, fmt.Sprintf("dbname=%s", conn.Database))
	}

	if conn.Username != "" {
		params = append(params, fmt.Sprintf("user=%s", conn.Username))
	}

	if conn.Password != "" {
		params = append(params, "password="+utils.QuotePQValue(conn.Password))
	}

	if conn.SSLMode != "" {
		params = append(params, fmt.Sprintf("sslmode=%s", conn.SSLMode))
	} else {
		params = append(params, "sslmode=prefer")
	}

	// Add SSL certificate parameters
	if conn.SSLCert != "" {
		params = append(params, fmt.Sprintf("sslcert=%s", conn.SSLCert))
	}

	if conn.SSLKey != "" {
		params = append(params, fmt.Sprintf("sslkey=%s", conn.SSLKey))
	}

	if conn.SSLRootCert != "" {
		params = append(params, fmt.Sprintf("sslrootcert=%s", conn.SSLRootCert))
	}

	// Add extra parameters
	for key, value := range conn.ExtraParams {
		params = append(params, fmt.Sprintf("%s=%s", key, value))
	}

	return strings.Join(params, " ")
}

// createTables creates the necessary tables if they don't exist
var recoveryIndexStatements = []string{
	`CREATE INDEX IF NOT EXISTS idx_health_events_recovery_identity ON health_events (` +
		`(document->'healthevent'->>'agent'), ` +
		`(COALESCE(document->'healthevent'->>'componentclass', ` +
		`document->'healthevent'->>'componentClass')), ` +
		`(COALESCE(document->'healthevent'->>'checkname', ` +
		`document->'healthevent'->>'checkName')), ` +
		`(COALESCE(document->'healthevent'->>'nodename', ` +
		`document->'healthevent'->>'nodeName')), ` +
		`(document->'healthevent'->>'version'), created_at, id)`,
	`CREATE INDEX IF NOT EXISTS idx_health_events_fault_quarantine_pending ` +
		`ON health_events (created_at, id) WHERE (` +
		`COALESCE(document->'healtheventstatus'->>'nodequarantined', ` +
		`document->'healtheventstatus'->>'nodeQuarantined') IS NULL OR ` +
		`COALESCE(document->'healtheventstatus'->>'nodequarantined', ` +
		`document->'healtheventstatus'->>'nodeQuarantined') = '' OR ` +
		`COALESCE(document->'healtheventstatus'->>'nodequarantined', ` +
		`document->'healtheventstatus'->>'nodeQuarantined') = 'NotStarted') AND (` +
		`document->'healtheventstatus'->>'faultquarantinerecovery' IS NULL OR ` +
		`document->'healtheventstatus'->>'faultquarantinerecovery' = '')`,
}

type runtimeUpgradeIndex struct {
	name            string
	createStatement string
	dropStatement   string
}

// runtimeUpgradeIndexes are checked on every datastore startup so existing
// databases receive indexes added after initial Helm provisioning.
var runtimeUpgradeIndexes = []runtimeUpgradeIndex{
	{
		name: "idx_health_events_analyzer_lookup",
		createStatement: `CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_health_events_analyzer_lookup ` +
			`ON health_events (node_name, event_type, created_at DESC, ` +
			`(document->'healthevent'->>'agent'))`,
		dropStatement: `DROP INDEX CONCURRENTLY IF EXISTS idx_health_events_analyzer_lookup`,
	},
}

const runtimeUpgradeIndexValidityQuery = `SELECT idx.indisvalid
	FROM pg_catalog.pg_index AS idx
	WHERE idx.indexrelid = pg_catalog.to_regclass($1)`

// startRuntimeUpgrades applies performance migrations outside the datastore
// startup critical path. Close cancels and joins the task before closing the
// connection pool; an interrupted concurrent index build is retried on the
// next startup by runRuntimeUpgrades.
func (p *PostgreSQLDataStore) startRuntimeUpgrades() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	p.runtimeUpgradeCancel = cancel
	p.runtimeUpgradeDone = done

	go func() {
		defer close(done)

		runRuntimeUpgrades(ctx, p.db)
	}()
}

func warnRuntimeUpgradeError(ctx context.Context, message, index string, err error) {
	if ctx.Err() != nil {
		return
	}

	slog.WarnContext(ctx, message, "index", index, "error", err)
}

// runRuntimeUpgrades executes each statement directly through the connection
// pool, outside an explicit transaction block. Do not wrap this function in
// BeginTx: PostgreSQL rejects CREATE INDEX CONCURRENTLY inside a transaction
// block.
func runRuntimeUpgrades(ctx context.Context, db *sql.DB) {
	for _, index := range runtimeUpgradeIndexes {
		var valid bool

		err := db.QueryRowContext(ctx, runtimeUpgradeIndexValidityQuery, index.name).Scan(&valid)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			warnRuntimeUpgradeError(ctx, "Failed to inspect PostgreSQL runtime upgrade index", index.name, err)

			continue
		}

		if err == nil && valid {
			continue
		}

		if err == nil {
			if _, err := db.ExecContext(ctx, index.dropStatement); err != nil {
				warnRuntimeUpgradeError(ctx,
					"Failed to remove invalid PostgreSQL runtime upgrade index", index.name, err)

				continue
			}
		}

		if _, err := db.ExecContext(ctx, index.createStatement); err != nil {
			warnRuntimeUpgradeError(ctx, "Failed to apply PostgreSQL runtime upgrade", index.name, err)
		}
	}
}

func createTables(ctx context.Context, db *sql.DB) error {
	schemas := []string{
		`CREATE EXTENSION IF NOT EXISTS "uuid-ossp"`,

		// Maintenance Events Table
		`CREATE TABLE IF NOT EXISTS maintenance_events (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			event_id VARCHAR(255) UNIQUE NOT NULL,
			csp VARCHAR(50) NOT NULL,
			cluster_name VARCHAR(255) NOT NULL,
			node_name VARCHAR(255),
			status VARCHAR(50) NOT NULL,
			csp_status VARCHAR(50),
			scheduled_start_time TIMESTAMPTZ,
			actual_end_time TIMESTAMPTZ,
			event_received_timestamp TIMESTAMPTZ NOT NULL,
			last_updated_timestamp TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			document JSONB NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,

		// Health Events Table
		`CREATE TABLE IF NOT EXISTS health_events (
			id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
			node_name VARCHAR(255) NOT NULL,
			event_type VARCHAR(100),
			severity VARCHAR(50),
			recommended_action VARCHAR(100),
			node_quarantined VARCHAR(50),
			user_pods_eviction_status VARCHAR(50) DEFAULT 'NotStarted',
			user_pods_eviction_message TEXT,
			fault_remediated BOOLEAN,
			last_remediation_timestamp TIMESTAMPTZ,
			document JSONB NOT NULL,
			created_at TIMESTAMPTZ DEFAULT NOW(),
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,

		// Change tracking table for polling-based change streams
		`CREATE TABLE IF NOT EXISTS datastore_changelog (
			id BIGSERIAL PRIMARY KEY,
			table_name VARCHAR(100) NOT NULL,
			record_id UUID NOT NULL,
			operation VARCHAR(20) NOT NULL,
			old_values JSONB,
			new_values JSONB,
			changed_at TIMESTAMPTZ DEFAULT NOW(),
			processed BOOLEAN DEFAULT FALSE
		)`,

		// Resume tokens table
		`CREATE TABLE IF NOT EXISTS resume_tokens (
			client_name VARCHAR(255) PRIMARY KEY,
			resume_token JSONB NOT NULL,
			last_updated TIMESTAMPTZ DEFAULT NOW()
		)`,
	}

	timestampColumns := []string{
		`ALTER TABLE health_events ADD COLUMN IF NOT EXISTS quarantine_finish_timestamp TIMESTAMPTZ`,
		`ALTER TABLE health_events ADD COLUMN IF NOT EXISTS drain_finish_timestamp TIMESTAMPTZ`,
	}

	indexes := []string{
		// Maintenance Events Indexes
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_event_id ` +
			`ON maintenance_events(event_id)`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_csp_cluster ` +
			`ON maintenance_events(csp, cluster_name)`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_node_status ` +
			`ON maintenance_events(node_name, status)`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_status_scheduled ` +
			`ON maintenance_events(status, scheduled_start_time) ` +
			`WHERE scheduled_start_time IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_status_actual_end ` +
			`ON maintenance_events(status, actual_end_time) ` +
			`WHERE actual_end_time IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_received_desc ` +
			`ON maintenance_events(csp, cluster_name, event_received_timestamp DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_maintenance_events_document_gin ` +
			`ON maintenance_events USING GIN (document)`,

		// Health Events Indexes
		`CREATE INDEX IF NOT EXISTS idx_health_events_node_name ON health_events(node_name)`,
		`CREATE INDEX IF NOT EXISTS idx_health_events_node_type ON health_events(node_name, event_type)`,
		`CREATE INDEX IF NOT EXISTS idx_health_events_quarantined ON health_events(node_quarantined) ` +
			`WHERE node_quarantined IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_health_events_created_desc ON health_events(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_health_events_created_id ON health_events(created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_health_events_document_gin ON health_events USING GIN (document)`,

		// Changelog Indexes
		`CREATE INDEX IF NOT EXISTS idx_changelog_unprocessed ON datastore_changelog(changed_at) WHERE processed = FALSE`,
		`CREATE INDEX IF NOT EXISTS idx_changelog_table_record ON datastore_changelog(table_name, record_id)`,
		// Composite index for timestamp-based resume (like MongoDB)
		// Supports: WHERE changed_at > $1 OR (changed_at = $1 AND id > $2)
		`CREATE INDEX IF NOT EXISTS idx_changelog_resume
			ON datastore_changelog(table_name, changed_at, id)
			WHERE processed = FALSE`,
	}
	indexes = append(indexes, recoveryIndexStatements...)

	// Execute schema creation
	for _, schema := range schemas {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("failed to create schema: %w", err)
		}
	}

	for _, timestampColumn := range timestampColumns {
		if _, err := db.ExecContext(ctx, timestampColumn); err != nil {
			return fmt.Errorf("failed to add timestamp column: %w", err)
		}
	}

	// Execute index creation
	for _, index := range indexes {
		if _, err := db.ExecContext(ctx, index); err != nil {
			slog.Warn("Failed to create index (may already exist)", "error", err)
		}
	}

	// Not fatal for this component: the deployment platform connector
	// verifies the index before it takes writes and stays unready, refusing
	// batches with a retryable status, until it exists.
	if err := ensureIdempotencyIndex(ctx, db); err != nil {
		slog.Error("Failed to ensure the health event idempotency index; the deployment platform connector "+
			"stays unready until it exists", "index", datastore.HealthEventIdempotencyIndexName, "error", err)
	}

	// Create change tracking triggers
	if err := createChangeTriggers(ctx, db); err != nil {
		return fmt.Errorf("failed to create change triggers: %w", err)
	}

	slog.Info("Successfully created PostgreSQL tables and indexes")

	return nil
}

// createChangeTriggers creates triggers for change tracking
func createChangeTriggers(ctx context.Context, db *sql.DB) error {
	triggerFunction := `
		CREATE OR REPLACE FUNCTION log_table_changes()
		RETURNS TRIGGER AS $$
		DECLARE
			changelog_id BIGINT;
		BEGIN
			IF TG_OP = 'DELETE' THEN
				INSERT INTO datastore_changelog (table_name, record_id, operation, old_values)
				VALUES (TG_TABLE_NAME, OLD.id, TG_OP, to_jsonb(OLD))
				RETURNING id INTO changelog_id;
				
				-- Send async notification for instant delivery
				PERFORM pg_notify(
					'nvsentinel_changes',
					json_build_object(
						'id', changelog_id,
						'table', TG_TABLE_NAME,
						'operation', TG_OP
					)::text
				);
				RETURN OLD;
			ELSIF TG_OP = 'UPDATE' THEN
				INSERT INTO datastore_changelog (table_name, record_id, operation, old_values, new_values)
				VALUES (TG_TABLE_NAME, NEW.id, TG_OP, to_jsonb(OLD), to_jsonb(NEW))
				RETURNING id INTO changelog_id;
				
				-- Send async notification for instant delivery
				PERFORM pg_notify(
					'nvsentinel_changes',
					json_build_object(
						'id', changelog_id,
						'table', TG_TABLE_NAME,
						'operation', TG_OP
					)::text
				);
				RETURN NEW;
			ELSIF TG_OP = 'INSERT' THEN
				INSERT INTO datastore_changelog (table_name, record_id, operation, new_values)
				VALUES (TG_TABLE_NAME, NEW.id, TG_OP, to_jsonb(NEW))
				RETURNING id INTO changelog_id;
				
				-- Send async notification for instant delivery
				PERFORM pg_notify(
					'nvsentinel_changes',
					json_build_object(
						'id', changelog_id,
						'table', TG_TABLE_NAME,
						'operation', TG_OP
					)::text
				);
				RETURN NEW;
			END IF;
			RETURN NULL;
		END;
		$$ LANGUAGE plpgsql;`

	triggers := []string{
		triggerFunction,
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_trigger
				WHERE tgname = 'maintenance_events_changes'
					AND tgrelid = 'maintenance_events'::regclass
					AND NOT tgisinternal
			) THEN
				CREATE TRIGGER maintenance_events_changes
					AFTER INSERT OR UPDATE OR DELETE ON maintenance_events
					FOR EACH ROW EXECUTE FUNCTION log_table_changes();
			END IF;
		EXCEPTION
			-- Another datastore may create the trigger after the existence check.
			WHEN duplicate_object THEN
				NULL;
		END;
		$$`,
		`DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1
				FROM pg_trigger
				WHERE tgname = 'health_events_changes'
					AND tgrelid = 'health_events'::regclass
					AND NOT tgisinternal
			) THEN
				CREATE TRIGGER health_events_changes
					AFTER INSERT OR UPDATE OR DELETE ON health_events
					FOR EACH ROW EXECUTE FUNCTION log_table_changes();
			END IF;
		EXCEPTION
			-- Another datastore may create the trigger after the existence check.
			WHEN duplicate_object THEN
				NULL;
		END;
		$$`,
	}

	for _, trigger := range triggers {
		if _, err := db.ExecContext(ctx, trigger); err != nil {
			return fmt.Errorf("failed to create trigger: %w", err)
		}
	}

	return nil
}

// setupLockKey is the advisory lock a component holds while it sets the
// tables up, so the setups run one at a time; any value, the same in every
// component. The idempotency index is built CONCURRENTLY, and PostgreSQL
// aborts such a build as the deadlock victim whenever another session's DDL
// on the table arrives during it, even a statement that changes nothing,
// after blocking that session for the rest of the build. Without the lock,
// components starting together would abort each other's build on a large
// table again and again.
const setupLockKey int64 = 2026091401

// setupLockPoll is how often a waiting component tries the lock again.
var setupLockPoll = time.Second

// withSetupLock runs setup while holding the setup lock. The lock is
// session-level: it lives on one connection and is released with it, so a
// component that dies mid-setup does not keep the others out. The setup's
// own statements run on other pooled connections, so the pool must not be
// limited to one connection yet when this runs. Waiters poll
// pg_try_advisory_lock instead of blocking in pg_advisory_lock, because a
// session blocked inside a statement holds a snapshot, and a concurrent index
// build in the holder's setup would wait for that snapshot while the waiter
// waits for the lock: the deadlock the lock is there to prevent.
func withSetupLock(ctx context.Context, db *sql.DB, setup func() error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection for the setup lock: %w", err)
	}

	defer conn.Close()

	for waited := false; ; waited = true {
		var locked bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", setupLockKey).Scan(&locked); err != nil {
			return fmt.Errorf("taking the setup lock: %w", err)
		}

		if locked {
			if waited {
				slog.Info("Setup lock acquired")
			}

			break
		}

		if !waited {
			slog.Info("Another component is setting the tables up; waiting for the setup lock")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(setupLockPoll):
		}
	}

	defer func() {
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1)", setupLockKey); err != nil {
			slog.Warn("Releasing the setup lock failed; the connection's close releases it", "error", err)
		}
	}()

	return setup()
}

// idempotencyIndexBuildAttempts and idempotencyIndexRetryDelay bound how often
// a CONCURRENTLY build of the idempotency index is tried again after
// PostgreSQL aborted it as a deadlock victim (see ensureIdempotencyIndex).
const idempotencyIndexBuildAttempts = 3

var idempotencyIndexRetryDelay = 2 * time.Second

// ensureIdempotencyIndex creates the deployment platform connector's
// idempotency index, or repairs it, without blocking the health event
// inserts of the other components: CONCURRENTLY, unlike the indexes above,
// because it is added to tables that are already large and written to.
// A build in progress is left alone; a leftover of a failed build (INVALID)
// or an index of another definition is dropped and built again. The result
// is verified, so a build another session aborted is reported rather than
// skipped.
//
// The setup lock keeps the components' setups from overlapping, so their
// DDL cannot abort this build. The retry below covers a component still on
// older code during an upgrade, whose setup takes strong locks on the table:
// PostgreSQL then aborts the concurrent build as the deadlock victim and
// leaves an INVALID index behind, and the next pass finds the index another
// session finished, leaves a build still running alone, or drops the leftover
// and builds once more.
//
// Documents that already share a key make the unique build fail for good: the
// error names the key and how to find the rows, and nothing is retried.
func ensureIdempotencyIndex(ctx context.Context, db *sql.DB) error {
	for attempt := 1; ; attempt++ {
		err := ensureIdempotencyIndexOnce(ctx, db)
		if pqErr, ok := pgError(err); ok && pqErr.Code == pqerror.UniqueViolation {
			return fmt.Errorf("the health events table holds documents that share an idempotency key, so the unique "+
				"index cannot be built until the extra rows are removed (%s); they are listed by: SELECT document #>> "+
				"'%s' AS key, count(*) FROM health_events GROUP BY 1 HAVING count(*) > 1: %w",
				pqErr.Detail, idempotencyKeyJSONPath, err)
		}

		if err == nil || attempt == idempotencyIndexBuildAttempts || !isDeadlockVictim(err) {
			return err
		}

		slog.Warn("Building the health event idempotency index was chosen as a deadlock victim by another "+
			"component's table setup; trying again",
			"index", datastore.HealthEventIdempotencyIndexName, "attempt", attempt, "error", err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(idempotencyIndexRetryDelay):
		}
	}
}

// idempotencyKeyJSONPath is the key's path in the stored document, as the
// index expression spells it.
const idempotencyKeyJSONPath = "{healthevent,metadata," + datastore.HealthEventIdempotencyKeyMetadataField + "}"

// pgError returns the PostgreSQL error inside err, if there is one.
func pgError(err error) (*pq.Error, bool) {
	return errors.AsType[*pq.Error](err)
}

// isDeadlockVictim reports whether PostgreSQL aborted the statement to
// resolve a deadlock.
func isDeadlockVictim(err error) bool {
	pqErr, ok := pgError(err)

	return ok && pqErr.Code == pqerror.TRDeadlockDetected
}

// ensureIdempotencyIndexOnce is one pass of ensureIdempotencyIndex.
func ensureIdempotencyIndexOnce(ctx context.Context, db *sql.DB) error {
	c := client.NewPostgreSQLClientFromDB(db, healthEventsTable)

	switch err := c.VerifyHealthEventIdempotencyIndex(ctx); {
	case err == nil:
		return nil
	case errors.Is(err, datastore.ErrIndexMissing):
	case errors.Is(err, datastore.ErrIndexMismatch):
		building, progressErr := c.IdempotencyIndexBuildInProgress(ctx)
		if progressErr != nil {
			return progressErr
		}

		if building {
			slog.Info("Health event idempotency index is being built by another session; leaving it alone",
				"index", datastore.HealthEventIdempotencyIndexName)

			return nil
		}

		if _, dropErr := db.ExecContext(ctx, client.DropIdempotencyIndexStatement); dropErr != nil {
			return fmt.Errorf("dropping the mismatched idempotency index: %w", dropErr)
		}
	default:
		return fmt.Errorf("checking the idempotency index: %w", err)
	}

	if _, err := db.ExecContext(ctx, client.CreateIdempotencyIndexStatement); err != nil {
		return fmt.Errorf("building the idempotency index: %w", err)
	}

	return c.VerifyHealthEventIdempotencyIndex(ctx)
}
