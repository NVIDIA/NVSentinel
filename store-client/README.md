# Store Client SDK

Database abstraction layer for NVSentinel components. Provides a unified interface for database operations supporting **both MongoDB and PostgreSQL** with change streams, transactions, domain-specific queries, and database-agnostic query builders.

## Quick Start

### Database-Agnostic Queries (Recommended)

**Use this approach for new code - works with both MongoDB and PostgreSQL**:

```go
import (
    "github.com/nvidia/nvsentinel/store-client/pkg/datastore"
    "github.com/nvidia/nvsentinel/store-client/pkg/query"
    _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
)

// Load configuration from environment
config, err := datastore.LoadDatastoreConfig()
if err != nil {
    return err
}

// Create datastore (automatically uses MongoDB or PostgreSQL based on DATASTORE_PROVIDER)
ds, err := datastore.NewDataStore(ctx, *config)
if err != nil {
    return err
}
defer ds.Close(ctx)

// Get health event store
healthStore := ds.HealthEventStore()

// Build database-agnostic query
q := query.New().Build(
    query.And(
        query.Eq("healthevent.nodename", "node-1"),
        query.Eq("healthevent.isfatal", true),
    ),
)

// Execute query - works with both MongoDB and PostgreSQL!
events, err := healthStore.FindHealthEventsByQuery(ctx, q)
if err != nil {
    return err
}

// Process events
for _, event := range events {
    fmt.Printf("Event: %s\n", event.HealthEvent.NodeName)
}
```

### Legacy DatabaseClient (Backward Compatible)

**For existing code using the old API**:

```go
import (
    "github.com/nvidia/nvsentinel/store-client/pkg/helper"
    _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb"
)

// Create database client only (no change streams)
dbClient, err := helper.NewDatabaseClientOnly(ctx, "my-module")
if err != nil {
    return err
}
defer dbClient.Close(ctx)

// Use MongoDB-style filters
filter := map[string]interface{}{"nodename": "node-1"}
cursor, err := dbClient.Find(ctx, filter, nil)
```

### With Change Streams

For modules that need to watch database changes:

```go
// Define your change stream pipeline
pipeline := []interface{}{
    map[string]interface{}{
        "$match": map[string]interface{}{
            "operationType": map[string]interface{}{
                "$in": []interface{}{"insert", "update"},
            },
        },
    },
}

// Create complete bundle (database + change stream watcher)
bundle, err := helper.NewDatastoreClient(ctx, helper.DatastoreClientConfig{
    ModuleName: "my-module",
    Pipeline:   pipeline,
})
if err != nil {
    return err
}
defer bundle.Close(ctx)

// Use database client
dbClient := bundle.DatabaseClient

// Use change stream watcher
watcher := bundle.ChangeStreamWatcher
watcher.Start(ctx)
for event := range watcher.Events() {
    // Process event
}
```

## Configuration

### Environment Variables

**Required**:
- `DATASTORE_PROVIDER` - Database provider (`mongodb` or `postgresql`)
- `DATASTORE_HOST` - Database host

**Provider-specific**:
- `DATASTORE_USERNAME` - Database username (PostgreSQL only)
- `DATASTORE_PASSWORD` - Database password (PostgreSQL only)

**Optional**:
- `DATASTORE_PORT` - Database port (defaults: 27017 for MongoDB, 5432 for PostgreSQL)
- `DATASTORE_DATABASE` - Database name (default: `nvsentinel`)
- `DATASTORE_SSLMODE` - SSL mode for connections
- `DATASTORE_SSLCERT` - Path to client certificate
- `DATASTORE_SSLKEY` - Path to client key
- `DATASTORE_SSLROOTCERT` - Path to CA certificate

**For change streams** (if needed):
- `RESUME_TOKEN_DATABASE` - Database for storing resume tokens
- `RESUME_TOKEN_COLLECTION` - Collection for resume tokens

**Example - PostgreSQL**:
```bash
export DATASTORE_PROVIDER=postgresql
export DATASTORE_HOST=localhost
export DATASTORE_PORT=5432
export DATASTORE_DATABASE=nvsentinel
export DATASTORE_USERNAME=nvsentinel
export DATASTORE_PASSWORD=secret
```

**Example - MongoDB** (default):
```bash
export DATASTORE_PROVIDER=mongodb
export DATASTORE_HOST=localhost
export DATASTORE_PORT=27017
export DATASTORE_DATABASE=nvsentinel
```

### TLS Certificates

The SDK looks for certificates in these locations (in order):
1. Custom path from `DATASTORE_SSLCERT` / `DATASTORE_SSLKEY` / `DATASTORE_SSLROOTCERT`
2. Path provided via `DatabaseClientCertMountPath` in config
3. Environment variable `MONGODB_CLIENT_CERT_MOUNT_PATH`
4. Legacy path `/etc/ssl/mongo-client` (backward compatibility)

## Key Interfaces

### DataStore (Recommended)
High-level database-agnostic interface - use this for new code:
```go
type DataStore interface {
    HealthEventStore() HealthEventStore
    MaintenanceEventStore() MaintenanceEventStore
    Close(ctx) error
}

type HealthEventStore interface {
    // Database-agnostic query methods (works with MongoDB and PostgreSQL)
    FindHealthEventsByQuery(ctx, builder QueryBuilder) ([]HealthEventWithStatus, error)
    UpdateHealthEventsByQuery(ctx, queryBuilder QueryBuilder, updateBuilder UpdateBuilder) error

    // Legacy methods (backward compatible)
    FindHealthEventsByFilter(ctx, filter) ([]HealthEventWithStatus, error)
    GetLatestEventForNode(ctx, nodeName) (*HealthEventWithStatus, error)
    // ... other methods ...
}
```

### QueryBuilder (Database-Agnostic)
Build queries that work with both databases:
```go
import "github.com/nvidia/nvsentinel/store-client/pkg/query"

// Build a query
q := query.New().Build(
    query.Or(
        query.Eq("status", "InProgress"),
        query.And(
            query.Gte("priority", 5),
            query.In("type", []interface{}{"critical", "warning"}),
        ),
    ),
)

// Query automatically converts to:
// MongoDB: {"$or": [{"status": "InProgress"}, {"$and": [...]}]}
// PostgreSQL: "(data->>'status' = $1) OR ((data->>'priority')::numeric >= $2 AND ...)"
```

**Supported Operators**:
- **Comparison**: `Eq`, `Ne`, `Gt`, `Gte`, `Lt`, `Lte`, `In`
- **Logical**: `And`, `Or`

### UpdateBuilder (Database-Agnostic)
Build updates that work with both databases:
```go
import "github.com/nvidia/nvsentinel/store-client/pkg/query"

// Build an update
u := query.NewUpdate().
    Set("status", "Completed").
    Set("completedAt", time.Now())

// Update automatically converts to:
// MongoDB: {"$set": {"status": "Completed", "completedAt": ...}}
// PostgreSQL: "data = jsonb_set(data, '{status}', '\"Completed\"'), ..."
```

### DatabaseClient (Legacy)
Core database operations - queries, updates, transactions:
```go
type DatabaseClient interface {
    Find(ctx, filter, options) (Cursor, error)
    FindOne(ctx, filter, options) (SingleResult, error)
    InsertMany(ctx, documents) (*InsertManyResult, error)
    UpdateDocument(ctx, filter, update) (*UpdateResult, error)
    UpdateManyDocuments(ctx, filter, update) (*UpdateResult, error)
    Aggregate(ctx, pipeline) (Cursor, error)
    WithTransaction(ctx, func) error
    Close(ctx) error
}
```

### ChangeStreamWatcher
Real-time event streaming from database changes:
```go
type ChangeStreamWatcher interface {
    Start(ctx)
    Events() <-chan Event              // Returns cached channel - safe to call multiple times
    MarkProcessed(ctx, token) error
    Close(ctx) error
}
```

**Note:** `Events()` returns a cached channel that's initialized once. Multiple calls to `Events()` return the same channel instance, making it safe to call directly in select loops and other control flow structures.

### EventProcessor
Unified event processing with retry logic and metrics:
```go
processor := client.NewEventProcessor(watcher, dbClient, config)
processor.SetEventHandler(func(ctx, event) error {
    // Your processing logic
    return nil
})
processor.Start(ctx)
```

## Common Patterns

### Query Examples

```go
// Simple equality (database-agnostic using query builder)
q := query.New().Build(
    query.And(
        query.Eq("healthevent.nodename", "node-1"),
        query.Eq("healthevent.isfatal", true),
    ),
)
events, err := healthStore.FindHealthEventsByQuery(ctx, q)

// Complex query with operators
q := query.New().Build(
    query.Or(
        query.Eq("status", "InProgress"),
        query.And(
            query.In("status", []interface{}{"NotStarted", ""}),
            query.Gte("priority", 5),
        ),
    ),
)
events, err := healthStore.FindHealthEventsByQuery(ctx, q)

// Legacy approach (MongoDB-style maps)
filter := map[string]interface{}{
    "healthevent.nodename": "node-1",
    "healthevent.isfatal":  true,
}
cursor, err := dbClient.Find(ctx, filter, nil)
```

### Update Operations

```go
// Database-agnostic updates (recommended)
q := query.New().Build(query.Eq("_id", eventID))
u := query.NewUpdate().
    Set("healtheventstatus.faultremediated", true).
    Set("healtheventstatus.remediatedAt", time.Now())

err := healthStore.UpdateHealthEventsByQuery(ctx, q, u)

// Legacy approach (MongoDB-style maps)
filter := map[string]interface{}{"_id": id}
update := map[string]interface{}{
    "$set": map[string]interface{}{
        "healtheventstatus.faultremediated": true,
    },
}
result, err := dbClient.UpdateDocument(ctx, filter, update)

// Bulk update (legacy API)
filter := map[string]interface{}{"healthevent.nodename": nodeName}
result, err := dbClient.UpdateManyDocuments(ctx, filter, update)
```

### Transactions

```go
err := dbClient.WithTransaction(ctx, func(sessCtx client.SessionContext) error {
    // All operations here are atomic
    _, err := dbClient.UpdateDocument(sessCtx, filter1, update1)
    if err != nil {
        return err
    }
    _, err = dbClient.InsertMany(sessCtx, documents)
    return err
})
```

## Architecture

```text
┌─────────────────────────────────────────────────────────┐
│  Your Module / Service                                   │
└─────────────────────────┬───────────────────────────────┘
                          │
                          ├─── Modern API (recommended) ──┐
                          │                                │
┌─────────────────────────▼───────────────────────────────┐
│  Query Builder (pkg/query/)                             │
│  • query.New().Build(...) - Fluent query API            │
│  • query.NewUpdate() - Fluent update API                │
│  • .ToMongo() / .ToSQL() - Dual output                  │
└─────────────────────────┬───────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────┐
│  DataStore Abstraction (pkg/datastore/)                 │
│  • DataStore.HealthEventStore()                         │
│  • FindHealthEventsByQuery(QueryBuilder)                │
│  • UpdateHealthEventsByQuery(QueryBuilder, UpdateBuilder)│
└─────────────────────────┬───────────────────────────────┘
                          │
              ┌───────────┴────────────┐
              │                        │
┌─────────────▼──────────┐  ┌─────────▼──────────────────┐
│  MongoDB Provider      │  │  PostgreSQL Provider       │
│  • Existing code       │  │  • Native SQL queries      │
│  • Zero changes        │  │  • JSONB operators         │
└────────────────────────┘  └────────────────────────────┘

                          │
                          ├─── Legacy API (backward compat) ──┐
                          │                                     │
┌─────────────────────────▼───────────────────────────────┐
│  DatabaseClient / Helper                                 │
│  • helper.NewDatabaseClientOnly()                        │
│  • helper.NewDatastoreClient()                           │
└─────────────────────────┬───────────────────────────────┘
                          │
┌─────────────────────────▼───────────────────────────────┐
│  Provider (MongoDB / PostgreSQL)                         │
└─────────────────────────────────────────────────────────┘
```

## Switching Between MongoDB and PostgreSQL

The SDK makes it trivial to switch database backends:

```bash
# Use MongoDB (default)
export DATASTORE_PROVIDER=mongodb
export DATASTORE_HOST=mongodb.example.com
./my-service

# Use PostgreSQL
export DATASTORE_PROVIDER=postgresql
export DATASTORE_HOST=postgresql.example.com
export DATASTORE_USERNAME=nvsentinel
export DATASTORE_PASSWORD=secret
./my-service
```

**No code changes needed** - the same service code works with both databases!

## Provider Registration

Import the providers you need to register them:

**MongoDB**:
```go
import _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb"
```

**PostgreSQL**:
```go
import _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/postgresql"
```

**Both**:
```go
import _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"
```

The underscore import ensures the provider's `init()` function runs and registers itself with the SDK.

## API Selection Guide

Choose the right API for your use case:

| Use Case | Recommended API | Why |
|----------|----------------|-----|
| **New code** | `DataStore` + `query.Builder` | Database-agnostic, type-safe, works with MongoDB and PostgreSQL |
| **Simple CRUD** | `DataStore.HealthEventStore()` | High-level, domain-specific operations |
| **Complex queries** | `query.Builder` with operators | Supports `$or`, `$and`, `$in`, `$gt`, etc. for both databases |
| **Existing code** | `DatabaseClient` + maps | Backward compatible, MongoDB-style filters |
| **Change streams** | `ChangeStreamWatcher` | Real-time event streaming |
| **Event processing** | `EventProcessor` | Built-in retry logic and metrics |

## PostgreSQL Support

**Full PostgreSQL support is available** via the query builder system:

✅ **What Works**:
- All CRUD operations (Find, Insert, Update, Delete)
- Complex queries using query builders (`$or`, `$and`, `$in`, `$gt`, `$lt`, etc.)
- Change streams (polling-based with 1-second latency)
- Nested JSONB field queries
- Batch operations
- Resume tokens

⚠️ **Limitations**:
- Complex aggregation pipelines (`$group`, `$setWindowFields`, `$facet`) not supported via DatabaseClient
- Change streams use polling (1-second latency) vs. MongoDB's real-time streams
- Transactions not fully implemented yet

**For detailed PostgreSQL documentation**, see:
- **[POSTGRESQL_IMPLEMENTATION.md](POSTGRESQL_IMPLEMENTATION.md)** - Complete PostgreSQL implementation guide

## Quick Troubleshooting

| Symptom | Likely Cause | Solution |
|---------|--------------|----------|
| `"unsupported provider: postgresql"` | Provider not imported | Add `_ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers"` |
| No change stream events (PostgreSQL) | Missing database triggers | Run `docs/postgresql-schema.sql` to create triggers |
| Resume token errors after restart | Stale tokens from schema change | Clear the `resume_tokens` table |
| `"data must be a map[string]interface{}"` | Incorrect filter format | Use query builders for type safety |
| Query returns no results (PostgreSQL) | Case sensitivity or type mismatch | Verify JSONB paths and value types match database data |
| `"unknown changestream mode"` | Invalid mode configuration | Use `ModeHybrid` (recommended) or `ModePolling` |

For detailed troubleshooting, see [POSTGRESQL_IMPLEMENTATION.md](POSTGRESQL_IMPLEMENTATION.md#troubleshooting).

## Further Reading

- **[POSTGRESQL_IMPLEMENTATION.md](POSTGRESQL_IMPLEMENTATION.md)** - PostgreSQL implementation details, architecture, and migration guide
- **[DEVELOPER_GUIDE.md](DEVELOPER_GUIDE.md)** - Comprehensive guide with advanced usage, best practices, and detailed examples
- **Examples in codebase:**
  - `node-drainer/main.go` - Database-agnostic queries with query builder
  - `health-events-analyzer/pkg/reconciler/reconciler.go` - Change streams with event processing
  - `node-drainer/pkg/initializer/init.go` - Database client initialization
  - `fault-quarantine/pkg/reconciler/reconciler.go` - Change streams with filtering
