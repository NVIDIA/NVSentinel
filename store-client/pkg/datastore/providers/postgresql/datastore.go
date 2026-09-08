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

	"github.com/XSAM/otelsql"
	_ "github.com/lib/pq" // PostgreSQL driver
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

	// Set connection pool settings
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(time.Hour)

	if _, err := otelsql.RegisterDBStatsMetrics(db,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
	); err != nil {
		slog.Warn("Failed to register DB stats metrics", "error", err)
	}

	if err := ValidateSchemaVersion(ctx, db); err != nil {
		db.Close()
		return nil, err
	}

	store := &PostgreSQLDataStore{
		db:                db,
		connString:        connectionString, // Store for LISTEN connections
		metricsRegisterer: config.MetricsRegisterer,
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
