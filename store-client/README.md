# Store Client SDK

Database abstraction layer for NVSentinel components. Provides a unified interface for database operations (MongoDB currently, PostgreSQL planned) with support for change streams, transactions, and domain-specific queries.

## Quick Start

### Basic Usage

Most modules use the helper package for initialization:

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

// Use the client
filter := map[string]interface{}{"nodename": "node-1"}
cursor, err := dbClient.Find(ctx, filter, nil)
```

### With Change Streams

For modules that need to watch database changes:

```go
// Define your change stream pipeline
pipeline := client.BuildNonFatalUnhealthyInsertsPipeline()

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

### Using Provided Config

If you already have a `DataStoreConfig`:

```go
config, err := datastore.LoadDatastoreConfig()
if err != nil {
    return err
}

bundle, err := helper.NewDatastoreClientFromConfig(
    ctx, "my-module", *config, pipeline,
)
if err != nil {
    return err
}
defer bundle.Close(ctx)
```

## Configuration

### Environment Variables

Required:
- `DATASTORE_PROVIDER` - Database provider (currently `mongodb`)
- `DATASTORE_HOST` - Database host
- `DATASTORE_USERNAME` - Database username
- `DATASTORE_PASSWORD` - Database password

Optional:
- `DATASTORE_PORT` - Database port (defaults: 27017 for MongoDB)
- `DATASTORE_DATABASE` - Database name (default: `nvsentinel`)
- `DATASTORE_SSLMODE` - SSL mode for connections
- `DATASTORE_SSLCERT` - Path to client certificate
- `DATASTORE_SSLKEY` - Path to client key
- `DATASTORE_SSLROOTCERT` - Path to CA certificate

For change streams (if needed):
- `RESUME_TOKEN_DATABASE` - Database for storing resume tokens
- `RESUME_TOKEN_COLLECTION` - Collection for resume tokens

### TLS Certificates

The SDK looks for certificates in these locations (in order):
1. Custom path from `DATASTORE_SSLCERT` / `DATASTORE_SSLKEY` / `DATASTORE_SSLROOTCERT`
2. Path provided via `DatabaseClientCertMountPath` in config
3. Environment variable `MONGODB_CLIENT_CERT_MOUNT_PATH`
4. Legacy path `/etc/ssl/mongo-client` (backward compatibility)

## Key Interfaces

### DatabaseClient
Core database operations - queries, updates, transactions:
```go
type DatabaseClient interface {
    Find(ctx, filter, options) (Cursor, error)
    FindOne(ctx, filter, options) (SingleResult, error)
    InsertMany(ctx, documents) (*InsertManyResult, error)
    UpdateDocument(ctx, filter, update) (*UpdateResult, error)
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
// Simple query
filter := map[string]interface{}{
    "healthevent.nodename": "node-1",
    "healthevent.isfatal": true,
}
cursor, err := dbClient.Find(ctx, filter, nil)

// Using filter builder
filter := client.NewFilterBuilder().
    Eq("nodename", "node-1").
    Eq("status", "active").
    Build()

// Aggregation
pipeline := client.BuildSequenceFacetPipeline(sequences)
cursor, err := dbClient.Aggregate(ctx, pipeline)
```

### Update Operations

```go
// Update single document
filter := map[string]interface{}{"_id": id}
update := map[string]interface{}{
    "$set": map[string]interface{}{
        "healtheventstatus.faultremediated": true,
    },
}
result, err := dbClient.UpdateDocument(ctx, filter, update)

// Bulk update
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
┌─────────────────┐
│  Your Module    │
└────────┬────────┘
         │
         ├─── helper.NewDatastoreClient() ──┐
         │                                   │
         ├─── DatabaseClient ────────────────┤
         │    (queries, updates)             │
         │                                   │
         ├─── ChangeStreamWatcher ──────────┤
         │    (real-time events)             │
         │                                   │
         └─── EventProcessor ────────────────┤
              (event pipeline)               │
                                             │
                                    ┌────────▼────────┐
                                    │  Provider       │
                                    │  (MongoDB)      │
                                    └─────────────────┘
```

## Provider Registration

Import the MongoDB provider to register it:
```go
import _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb"
```

The underscore import ensures the provider's `init()` function runs and registers itself with the SDK.

## Further Reading

- **[DEVELOPER_GUIDE.md](DEVELOPER_GUIDE.md)** - Comprehensive guide with advanced usage, best practices, and detailed examples
- **Examples in codebase:**
  - `health-events-analyzer/pkg/reconciler/reconciler.go` - Change streams with event processing
  - `node-drainer/pkg/initializer/init.go` - Database client with queries
  - `fault-quarantine/pkg/reconciler/reconciler.go` - Change streams with filtering
