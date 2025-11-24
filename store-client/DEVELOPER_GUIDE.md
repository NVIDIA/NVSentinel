# Store Client SDK - Developer Guide

## Overview

The Store Client SDK provides database-agnostic abstractions for interacting with health and maintenance event data in NVSentinel. This guide covers how to use the SDK for common development scenarios.

The SDK abstracts away database-specific details, allowing you to write code once and run it against different database backends (currently MongoDB, with PostgreSQL support planned). This is particularly useful for testing, deployment flexibility, and future migrations.

## Core Architecture

The SDK is built around four main interfaces that handle different aspects of data access:

### Key Interfaces

- **DatabaseClient**: Core database operations (Find, Insert, Update, Delete) - use this for custom queries and basic CRUD operations
  - Single document operations: `UpdateDocument`, `UpsertDocument`, `FindOne`
  - Bulk operations: `UpdateManyDocuments` - atomically update multiple documents matching a filter
  - Queries: `Find`, `CountDocuments`, `Aggregate`
  - Transactions: `WithTransaction` - execute multiple operations atomically
- **ChangeStreamWatcher**: Real-time event streaming from database changes - monitors database for live updates
- **EventProcessor**: Unified event processing with retry logic and metrics - handles the processing pipeline for incoming events
- **DataStore**: High-level domain-specific operations for health/maintenance events - provides business-logic focused methods

### Provider Pattern

The SDK uses a provider pattern to support multiple database backends while maintaining a consistent API. You import the provider you need, and the SDK handles the rest:

```go
// Register provider (done automatically by importing)
import _ "github.com/nvidia/nvsentinel/store-client/pkg/datastore/providers/mongodb"

// Create factory from environment
factory, err := helper.CreateClientFactory(config)
if err != nil {
    return err
}

// Get database-agnostic client
dbClient, err := factory.CreateDatabaseClient(ctx)
if err != nil {
    return err
}
```

### Health Event Lifecycle and Status

Health events in NVSentinel follow a defined lifecycle with specific status values:

**Quarantine Statuses:**
- `Quarantined`: Node has been quarantined due to a health issue
- `AlreadyQuarantined`: Additional health issue detected on already quarantined node
- `UnQuarantined`: Node has been unquarantined after health restored
- `Cancelled`: Quarantine was cancelled due to manual intervention (e.g., manual uncordon)

**Operation Statuses (for eviction/remediation):**
- `NotStarted`: Operation has not yet begun
- `InProgress`: Operation is currently running
- `Succeeded`: Operation completed successfully
- `Failed`: Operation encountered an error
- `AlreadyDrained`: Node was already in drained state

The `Cancelled` status is particularly important for handling manual overrides of automated processes. When an operator manually uncordons a node, all events in the current quarantine session are marked as `Cancelled` to prevent further automated actions.

## Common Use Cases

This section covers the most frequent scenarios you'll encounter when building NVSentinel components.

### 1. Basic Database Operations

These are your everyday CRUD operations. Use the high-level DataStore for business logic, or the low-level DatabaseClient for custom queries.

#### Reading Events

```go
// Using the high-level DataStore interface
dataStore, err := helper.CreateDataStore(ctx, config)
if err != nil {
    return err
}

// Get latest health event for a node
event, err := dataStore.HealthEventStore().GetLatestEventForNode(ctx, "node-1")
if err != nil {
    return err
}

// Using low-level DatabaseClient for custom queries
filter := client.NewFilterBuilder().
    Eq("nodeName", "node-1").
    Eq("status", "active").
    Build()

result, err := dbClient.FindOne(ctx, filter, nil)
if err != nil {
    return err
}
```

#### Writing Events

```go
// Create a health event
healthEvent := &model.HealthEventWithStatus{
    HealthEvent: model.HealthEvent{
        NodeName:  "node-1",
        CheckName: "gpu-check",
        // ... other fields
    },
    HealthEventStatus: model.HealthEventStatus{
        NodeQuarantined: &timestamp,
    },
}

// Insert using DataStore
err := dataStore.HealthEventStore().CreateEvent(ctx, healthEvent)
if err != nil {
    return err
}

// Or using low-level client
doc := client.ConvertToDocument(healthEvent)
_, err = dbClient.InsertOne(ctx, doc)
```

#### Updating Events

```go
// Update a single document using builder pattern
filter := client.NewFilterBuilder().Eq("_id", eventID).Build()
update := client.NewUpdateBuilder().
    Set("status", "resolved").
    Set("resolvedAt", time.Now()).
    Build()

_, err := dbClient.UpdateDocument(ctx, filter, update)

// Update multiple documents matching criteria
multiFilter := client.NewFilterBuilder().
    Eq("nodeName", "node-1").
    In("status", []string{"quarantined", "alreadyQuarantined"}).
    Build()

result, err := dbClient.UpdateManyDocuments(ctx, multiFilter, update)
if err != nil {
    return err
}
log.Printf("Updated %d documents", result.ModifiedCount)
```

### 2. Real-time Event Processing

Most NVSentinel components need to react to database changes in real-time. The SDK provides two approaches: simple event processing for most cases, and queue-based processing for high-throughput scenarios.

#### Setting up Change Stream Watching

Use this pattern when you need to process events as they arrive, with built-in retry logic and metrics:

```go
// Create change stream watcher for quarantine status changes
// Note: Include all relevant statuses including Cancelled for manual overrides
watcher, err := factory.CreateChangeStreamWatcher(ctx, &client.ChangeStreamConfig{
    Collection: "health_events",
    Pipeline: client.NewPipelineBuilder().
        Match(map[string]interface{}{
            "operationType": map[string]interface{}{"$in": []string{"insert", "update"}},
            "fullDocument.healtheventstatus.nodequarantined": map[string]interface{}{
                "$in": []model.Status{
                    model.Quarantined,
                    model.UnQuarantined,
                    model.Cancelled,  // Important: watch for manual cancellations
                },
            },
        }).
        Build(),
    ResumeAfter: lastProcessedToken,
})
if err != nil {
    return err
}

// Create event processor with retry logic
processor := client.NewEventProcessor(watcher, dbClient, client.EventProcessorConfig{
    MaxRetries:     3,
    RetryDelay:     time.Second * 2,
    EnableMetrics:  true,
    MetricsLabels:  map[string]string{"module": "my-module"},
})

// Set event handler
processor.SetEventHandler(client.EventHandlerFunc(func(ctx context.Context, event *model.HealthEventWithStatus) error {
    // Process the event
    log.Printf("Processing event for node: %s, status: %v",
        event.NodeName, event.HealthEventStatus.NodeQuarantined)

    // Handle different statuses
    if event.HealthEventStatus.NodeQuarantined != nil {
        switch *event.HealthEventStatus.NodeQuarantined {
        case model.Quarantined:
            return handleQuarantine(ctx, event)
        case model.UnQuarantined:
            return handleUnquarantine(ctx, event)
        case model.Cancelled:
            // Handle manual cancellation - stop automated actions
            return handleCancellation(ctx, event)
        }
    }

    return nil
}))

// Start processing
if err := processor.Start(ctx); err != nil {
    return err
}
```

#### Queue-based Event Processing

For high-throughput scenarios where you need multiple workers processing events concurrently:

```go
// Create queue-based processor
queueProcessor := client.NewQueueEventProcessor(watcher, client.QueueEventProcessorConfig{
    WorkerCount:    5,
    MaxRetries:     3,
    RetryDelay:     time.Second * 2,
    EnableMetrics:  true,
})

queueProcessor.SetEventHandler(client.EventHandlerFunc(handleEvent))
if err := queueProcessor.Start(ctx); err != nil {
    return err
}
```

### 3. Configuration Patterns

#### Environment-based Configuration

```go
// Automatic configuration from environment variables
config := &datastore.Config{
    ModuleName: "my-module",
    // DatabaseClientCertMountPath will be loaded from MONGODB_CLIENT_CERT_MOUNT_PATH
}

factory, err := helper.CreateClientFactory(config)
```

#### Custom Configuration

```go
// Custom MongoDB configuration
factory, err := storefactory.NewClientFactoryFromConnectionString(
    "mongodb://localhost:27017/nvsentinel",
    &storefactory.MongoConfig{
        Database:   "nvsentinel",
        MaxPoolSize: 50,
        Timeout:     30 * time.Second,
    },
)
```

### 4. Testing Patterns

#### Unit Testing with Mocks

```go
func TestMyComponent(t *testing.T) {
    // Use test utilities
    mockWatcher := testutils.NewMockWatcher()

    // Create test events
    testEvent := testutils.NewTestEventBuilder().
        WithNodeName("test-node").
        WithCheckName("test-check").
        WithHealthStatus(false, true).
        Build()

    // Send test event
    mockWatcher.SendEvent(testEvent)

    // Test your component
    processor := NewMyProcessor(mockWatcher, mockDB)
    err := processor.ProcessEvent(ctx, testEvent)
    assert.NoError(t, err)
}
```

#### Integration Testing

```go
func TestWithRealDatabase(t *testing.T) {
    // Create factory for testing
    factory, err := helper.CreateClientFactory(&datastore.Config{
        ModuleName: "test",
    })
    require.NoError(t, err)

    // Clean setup
    defer factory.Close()

    // Run tests...
}
```

## Module Integration Patterns

These patterns show how existing NVSentinel modules use the SDK. Choose the pattern that best matches your component's role in the system.

### 1. Health Event Analyzer Pattern

Use this pattern for components that analyze incoming health events and make decisions or trigger actions based on the analysis:

```go
// Typical analyzer setup
type Analyzer struct {
    dataStore    datastore.DataStore
    processor    client.EventProcessor
}

func NewAnalyzer(config *Config) (*Analyzer, error) {
    // Create data store
    dataStore, err := helper.CreateDataStore(ctx, &config.DatastoreConfig)
    if err != nil {
        return nil, err
    }

    // Create change stream watcher
    factory := dataStore.GetFactory()
    watcher, err := factory.CreateChangeStreamWatcher(ctx, &client.ChangeStreamConfig{
        Collection: "health_events",
        Pipeline:   buildAnalysisPipeline(),
    })
    if err != nil {
        return nil, err
    }

    // Create processor
    processor := client.NewEventProcessor(watcher, dataStore.GetDatabaseClient(),
        client.EventProcessorConfig{
            MaxRetries:    3,
            EnableMetrics: true,
            MetricsLabels: map[string]string{"module": "analyzer"},
        })

    analyzer := &Analyzer{
        dataStore: dataStore,
        processor: processor,
    }

    processor.SetEventHandler(client.EventHandlerFunc(analyzer.analyzeEvent))
    return analyzer, nil
}

func (a *Analyzer) analyzeEvent(ctx context.Context, event *model.HealthEventWithStatus) error {
    // Analyze health event
    if shouldTriggerAlert(event) {
        return a.createAlert(ctx, event)
    }
    return nil
}
```

### 2. Reconciler Pattern

Use this pattern for Kubernetes controllers that need to reconcile cluster state with health event data:

```go
// Controller reconciler using store client
type Reconciler struct {
    client.Client
    dataStore datastore.DataStore
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // Get latest health event for node
    event, err := r.dataStore.HealthEventStore().GetLatestEventForNode(ctx, req.Name)
    if err != nil {
        return ctrl.Result{}, err
    }

    // Update status based on event
    if event != nil {
        return r.handleHealthEvent(ctx, event)
    }

    return ctrl.Result{}, nil
}

func (r *Reconciler) handleHealthEvent(ctx context.Context, event *model.HealthEventWithStatus) error {
    // Update database status
    filter := client.NewFilterBuilder().Eq("_id", event.ID).Build()
    update := client.NewUpdateBuilder().
        Set("healtheventstatus.lastProcessed", time.Now()).
        Build()

    _, err := r.dataStore.GetDatabaseClient().UpdateOne(ctx, filter, update, nil)
    return err
}
```

### 3. Platform Connector Pattern

Use this pattern for components that bridge NVSentinel with external systems (monitoring, alerting, ticketing systems):

```go
// Platform connector for external systems
type StoreConnector struct {
    dataStore datastore.DataStore
    processor client.EventProcessor
}

func NewStoreConnector(config *Config) (*StoreConnector, error) {
    dataStore, err := helper.CreateDataStore(ctx, &config.DatastoreConfig)
    if err != nil {
        return nil, err
    }

    // Watch for specific event types
    factory := dataStore.GetFactory()
    watcher, err := factory.CreateChangeStreamWatcher(ctx, &client.ChangeStreamConfig{
        Collection: "health_events",
        Pipeline: client.NewPipelineBuilder().
            Match(map[string]interface{}{
                "healthevent.severity": map[string]interface{}{"$gte": "critical"},
            }).
            Build(),
    })
    if err != nil {
        return nil, err
    }

    processor := client.NewEventProcessor(watcher, dataStore.GetDatabaseClient(),
        client.EventProcessorConfig{
            MaxRetries:    5,
            RetryDelay:    time.Second * 5,
            EnableMetrics: true,
        })

    connector := &StoreConnector{
        dataStore: dataStore,
        processor: processor,
    }

    processor.SetEventHandler(client.EventHandlerFunc(connector.forwardEvent))
    return connector, nil
}
```

## Best Practices

Follow these practices to build robust, maintainable components that work well in production environments.

### 1. Error Handling

The SDK provides typed errors that help you handle different failure scenarios appropriately:

```go
// Use typed errors for better error handling
if client.IsNoDocumentsError(err) {
    // Handle no documents found
    return nil, nil
}

if datastore.IsValidationError(err) {
    // Handle validation errors
    log.Warn("Validation failed", "error", err)
    return nil, err
}

// Use error metadata for debugging
if validationErr, ok := err.(*datastore.ValidationError); ok {
    log.Error("Validation failed",
        "provider", validationErr.Provider,
        "metadata", validationErr.Metadata)
}
```

### 2. Resource Management

```go
// Always close resources
defer factory.Close()
defer processor.Stop(ctx)

// Use context for cancellation
ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
```

### 3. Metrics and Observability

```go
// Enable metrics in processors
processor := client.NewEventProcessor(watcher, dbClient, client.EventProcessorConfig{
    EnableMetrics: true,
    MetricsLabels: map[string]string{
        "module":     "my-module",
        "component":  "processor",
        "version":    buildVersion,
    },
})
```

### 4. Query Optimization

```go
// Use specific filters to reduce data transfer
filter := client.NewFilterBuilder().
    Eq("nodeName", nodeName).
    Gte("createdAt", time.Now().Add(-24*time.Hour)). // Only recent events
    Build()

// Use projections to limit fields
opts := &client.FindOptions{
    Projection: map[string]interface{}{
        "nodeName":      1,
        "status":        1,
        "createdAt":     1,
        "_id":           0, // Exclude large ID field if not needed
    },
}
```

### 5. Handling Manual Interventions

Always check for the `Cancelled` status in your event processors to respect manual operator interventions:

```go
// In reconcilers and processors, check for cancellation early
func (r *Reconciler) ProcessEvent(ctx context.Context, event *model.HealthEventWithStatus) error {
    // Check if event was cancelled by manual intervention
    if event.HealthEventStatus.NodeQuarantined != nil &&
        *event.HealthEventStatus.NodeQuarantined == model.Cancelled {
        log.Info("Event cancelled by manual intervention, skipping automated actions",
            "node", event.NodeName, "eventID", event.ID)
        // Clean up any in-progress operations for this node
        return r.cleanupAutomatedActions(ctx, event.NodeName)
    }

    // Proceed with normal processing
    return r.handleEvent(ctx, event)
}
```

When implementing change stream watchers, include `Cancelled` in your pipeline filters to detect manual interventions in real-time and stop automated remediation workflows immediately.

## Advanced Scenarios

These patterns are for specialized use cases where the standard SDK components don't meet your specific requirements. Use these when you need fine-grained control over processing logic or when integrating with systems that have unique constraints.

### Bulk Event Cancellation Pattern

When you need to cancel multiple related events (e.g., cancelling all quarantine events for a node due to manual intervention), use the UpdateManyDocuments method. This pattern is used in the manual uncordon feature:

```go
// Find the latest quarantine event to determine session start time
func CancelLatestQuarantiningEvents(ctx context.Context, nodeName string) error {
    // Step 1: Find the latest Quarantined event
    filter := map[string]interface{}{
        "healthevent.nodename": nodeName,
        "healtheventstatus.nodequarantined": map[string]interface{}{
            "$in": []model.Status{model.Quarantined, model.UnQuarantined},
        },
    }

    findOptions := &client.FindOneOptions{
        Sort: map[string]interface{}{"createdAt": -1},
    }

    var latestEvent struct {
        ID                string    `bson:"_id"`
        CreatedAt         time.Time `bson:"createdAt"`
        HealthEventStatus struct {
            NodeQuarantined *model.Status `bson:"nodequarantined"`
        } `bson:"healtheventstatus"`
    }

    result, err := databaseClient.FindOne(ctx, filter, findOptions)
    if err != nil {
        if client.IsNoDocumentsError(err) {
            // No events to cancel
            return nil
        }
        return err
    }

    if err := result.Decode(&latestEvent); err != nil {
        return err
    }

    // Step 2: Only proceed if the node is currently quarantined
    if latestEvent.HealthEventStatus.NodeQuarantined == nil ||
        *latestEvent.HealthEventStatus.NodeQuarantined != model.Quarantined {
        return nil
    }

    // Step 3: Cancel all events from the current quarantine session
    updateFilter := map[string]interface{}{
        "healthevent.nodename": nodeName,
        "createdAt":            map[string]interface{}{"$gte": latestEvent.CreatedAt},
        "healtheventstatus.nodequarantined": map[string]interface{}{
            "$in": []model.Status{model.Quarantined, model.AlreadyQuarantined},
        },
    }

    update := map[string]interface{}{
        "$set": map[string]interface{}{
            "healtheventstatus.nodequarantined": model.Cancelled,
        },
    }

    updateResult, err := databaseClient.UpdateManyDocuments(ctx, updateFilter, update)
    if err != nil {
        return err
    }

    log.Printf("Cancelled %d quarantine events for node %s",
        updateResult.ModifiedCount, nodeName)
    return nil
}
```

This pattern demonstrates:
- Finding the session boundary (latest quarantine event)
- Validating current state before making changes
- Using UpdateManyDocuments to atomically update all related events
- Proper error handling for edge cases (no documents, already unquarantined)

### Custom Event Processing

When the built-in EventProcessor doesn't fit your needs (e.g., custom retry logic, specialized error handling, or integration with external systems), you can implement your own processing loop:

```go
// Implement custom event processor for specialized logic
type CustomProcessor struct {
    watcher   client.ChangeStreamWatcher
    dbClient  client.DatabaseClient
    handler   EventHandler
}

func (p *CustomProcessor) Start(ctx context.Context) error {
    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case event, ok := <-p.watcher.Events():
            if !ok {
                return nil
            }

            // Custom processing logic
            if err := p.processWithCustomLogic(ctx, event); err != nil {
                log.Error("Processing failed", "error", err)
                // Custom retry or dead letter logic
            }
        }
    }
}
```

### Database Provider Extension

If you need to add support for a new database backend (e.g., PostgreSQL, Redis), you'll implement the core interfaces and register your provider. This allows existing code to work unchanged with the new backend:

```go
// To add a new database provider, implement these interfaces:
type MyDatabaseClient struct {
    // Implementation
}

func (c *MyDatabaseClient) FindOne(ctx context.Context, filter interface{}, opts *client.FindOptions) (client.SingleResult, error) {
    // Convert filter to provider-specific format
    // Execute query
    // Return wrapped result
}

// Register the provider
func init() {
    datastore.RegisterProvider("my-database", &MyProvider{})
}
```

This guide provides the foundation for working with the Store Client SDK. The patterns shown here are proven in production and will help you build robust, maintainable components that integrate seamlessly with the NVSentinel ecosystem.

For specific implementation details and real-world examples, refer to the existing module code in `fault-quarantine-module`, `health-events-analyzer`, `node-drainer-module`, and `platform-connectors`.