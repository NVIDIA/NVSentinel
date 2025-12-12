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
	"fmt"
	"log/slog"
	"strings"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

// PostgreSQLDataStore implements the DataStore interface for PostgreSQL
type PostgreSQLDataStore struct {
	db                    *sql.DB
	connString            string // Connection string for creating LISTEN connections
	maintenanceEventStore datastore.MaintenanceEventStore
	healthEventStore      datastore.HealthEventStore
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

	db, err := sql.Open("postgres", connectionString)
	if err != nil {
		return nil, fmt.Errorf("failed to open PostgreSQL connection: %w", err)
	}

	// Test connection
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping PostgreSQL database: %w", err)
	}

	// Set connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)

	// Create tables if they don't exist
	if err := createTables(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %w", err)
	}

	store := &PostgreSQLDataStore{
		db:         db,
		connString: connectionString, // Store for LISTEN connections
	}
	store.maintenanceEventStore = NewPostgreSQLMaintenanceEventStore(db)
	store.healthEventStore = NewPostgreSQLHealthEventStore(db)

	slog.Info("Successfully connected to PostgreSQL database", "host", config.Connection.Host)

	return store, nil
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
//
//nolint:cyclop // Watcher setup requires multiple configuration checks
func (p *PostgreSQLDataStore) NewChangeStreamWatcher(
	ctx context.Context, config interface{},
) (datastore.ChangeStreamWatcher, error) {
	// Convert the generic config to PostgreSQL-specific parameters
	var clientName, tableName string

	var hasPipeline bool

	// Handle different config types that might be passed

	switch c := config.(type) {
	case map[string]interface{}:
		// Support generic config format from factory
		if clientNameVal, ok := c["ClientName"].(string); ok {
			clientName = clientNameVal
		}

		if tableNameVal, ok := c["TableName"].(string); ok {
			tableName = tableNameVal
		}

		// Also support MongoDB-style CollectionName for compatibility
		if collectionName, ok := c["CollectionName"].(string); ok {
			tableName = collectionName
		}

		// Check if pipeline was provided (for warning purposes)
		if pipeline, ok := c["Pipeline"]; ok && pipeline != nil {
			hasPipeline = true
		}
	default:
		return nil, fmt.Errorf("unsupported config type: %T", config)
	}

	// Validate required parameters
	if clientName == "" {
		return nil, fmt.Errorf("ClientName is required")
	}

	if tableName == "" {
		return nil, fmt.Errorf("TableName (or CollectionName) is required")
	}

	// Create pipeline filter if pipeline was provided
	// Two-layer filtering for optimal performance:
	// 1. Server-side: SQL WHERE clause (built from raw pipeline in fetchNewChanges)
	// 2. Application-side: PipelineFilter (handles edge cases SQL can't express)
	var pipelineFilter *PipelineFilter

	var pipeline interface{}

	if hasPipeline {
		if c, ok := config.(map[string]interface{}); ok {
			pipeline = c["Pipeline"]
		}

		filter, err := NewPipelineFilter(pipeline)
		if err != nil {
			slog.Warn("Failed to parse MongoDB pipeline for PostgreSQL filtering",
				"error", err,
				"tableName", tableName,
				"clientName", clientName,
				"action", "all events will be returned without filtering")
		} else if filter != nil {
			pipelineFilter = filter
			slog.Info("PostgreSQL change stream will filter events using parsed MongoDB pipeline",
				"tableName", tableName,
				"clientName", clientName,
				"stages", len(filter.stages))
		}
	}

	// Convert PascalCase table name to snake_case for PostgreSQL compatibility
	snakeCaseTableName := toSnakeCase(tableName)

	slog.Info("Creating PostgreSQL changestream watcher",
		"originalTableName", tableName,
		"postgresTableName", snakeCaseTableName,
		"clientName", clientName)

	// Create and return PostgreSQL change stream watcher
	// Default to hybrid mode for best performance and reliability
	watcher := NewPostgreSQLChangeStreamWatcher(p.db, clientName, snakeCaseTableName, p.connString, ModeHybrid)
	watcher.pipeline = pipeline             // Store raw pipeline for SQL filter building
	watcher.pipelineFilter = pipelineFilter // Application-side filter as fallback

	// Wrap the watcher to provide Unwrap() support for backward compatibility
	return NewPostgreSQLChangeStreamWatcherWithUnwrap(watcher), nil
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
	ctx context.Context, clientName string, pipeline interface{},
) (datastore.ChangeStreamWatcher, error) {
	config := map[string]interface{}{
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
		params = append(params, fmt.Sprintf("password=%s", conn.Password))
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

	// Execute schema creation
	for _, schema := range schemas {
		if _, err := db.ExecContext(ctx, schema); err != nil {
			return fmt.Errorf("failed to create schema: %w", err)
		}
	}

	// Execute index creation
	for _, index := range indexes {
		if _, err := db.ExecContext(ctx, index); err != nil {
			slog.Warn("Failed to create index (may already exist)", "error", err)
		}
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
		`DROP TRIGGER IF EXISTS maintenance_events_changes ON maintenance_events`,
		`CREATE TRIGGER maintenance_events_changes
			AFTER INSERT OR UPDATE OR DELETE ON maintenance_events
			FOR EACH ROW EXECUTE FUNCTION log_table_changes()`,
		`DROP TRIGGER IF EXISTS health_events_changes ON health_events`,
		`CREATE TRIGGER health_events_changes
			AFTER INSERT OR UPDATE OR DELETE ON health_events
			FOR EACH ROW EXECUTE FUNCTION log_table_changes()`,
	}

	for _, trigger := range triggers {
		if _, err := db.ExecContext(ctx, trigger); err != nil {
			return fmt.Errorf("failed to create trigger: %w", err)
		}
	}

	return nil
}
