# Health Event Processing Strategy 

## Overview 

Currently, all health monitors publish events that downstream modules EXECUTE_REMEDIATION by default, which leads to the operations that affects the cluster state:
1. Node conditions are created/updated
2. Quarantine labels and annotations are applied even when fault-handling modules are configured to be **observability-only** (i.e., not performing cluster actions), creating confusion about why the node was not cordoned or why the remediation was not performed
3. Nodes are drained by node drainer module
4. Nodes are remediated by fault remediation

We want to support observability-only workflows without affecting cluster state. 

This feature will be implemented in all health monitors:
1. GPU Health Monitor
2. Syslog Health Monitor
3. CSP Health Monitor
4. Kubernetes Object Monitor
5. Health events analyzer

When a health monitor wants observability-only behavior, it publishes health events with `processingStrategy=STORE_ONLY`.

### EXECUTE_REMEDIATION Events Behaviour
`EXECUTE_REMEDIATION` means the default behavior: downstream modules EXECUTE_REMEDIATION the event and also modify cluster state (e.g., create/update node conditions, create Kubernetes events, quarantine, drain, or remediate).

### STORE_ONLY Events Behavior

`STORE_ONLY` events WILL be:
- Stored in database (for analysis)
- Exported as metrics to monitor which rule/policy is running in observability-only mode (STORE_ONLY):
	 **kubernetes-object-monitor**: Export `k8s_object_monitor_health_events_processing_strategy_overrides_total` (labels: `policy_name`, `strategy`) to track policies overridden to `STORE_ONLY`.
	**health-events-analyzer**: Export `health_event_analyzer_processing_strategy_overrides_total` (labels: `rule_name`, `strategy`) to track rules overridden to `STORE_ONLY`.
- Exported by event exporter (for external monitoring)

`STORE_ONLY` events will NOT trigger:
- Node condition creation/updates (Platform Connector skips)
- Kubernetes event creation (Platform Connector skips)
- Node quarantine (fault-quarantine skips - no taint, labels, or annotations)
- Node draining (node-drainer module won't receive them - filtered by pipeline)
- Remediation CR creation (fault-remediation module won't receive them - filtered by pipeline)
- Pattern analysis (health-events-analyzer ignores incoming `processingStrategy=STORE_ONLY` events for pattern detection and does not consider them while running the query)

---

## Flow Diagrams

```plaintext
			┌─────────────────────────────────────────────────────────────────┐
			│ Health Monitor                                                  │
			│ - Detects issue (XID error, CSP maintenance, etc.)              │
			│ - Selects processingStrategy (EXECUTE_REMEDIATION or STORE_ONLY)│
			│ - Creates HealthEvent with processingStrategy = STORE_ONLY      │
			└────────────────────────┬────────────────────────────────────────┘
									 ↓
			┌─────────────────────────────────────────────────────────────────┐
			│ Platform Connector                                              │
			│ - Receives event via gRPC                                       │
			│ - Runs pipeline transformers (overrides, metadata)              │
			│ - Checks processingStrategy                                     │
			│ -    SKIP node conditions (STORE_ONLY)                          │
			│ -    SKIP Kubernetes events (STORE_ONLY)                        │
			│ -    Stores in database (for observability)                     │
			└────────────────────────┬────────────────────────────────────────┘
									 ↓
			┌────────────────────────────────────────────────────────────────────────────┐
			│ Database (MongoDB/PostgreSQL)                                              │
			│ - Event stored with processingStrategy = STORE_ONLY                        │
			│ - Change stream triggered                                       			 │
			└─────────┬──────────────────────┬───────────────────────────┬───────────────┘
					  ↓                      ↓                           ↓
┌────────────────────────────┐  ┌────────────────────────────┐  ┌────────────────────────────┐
│ Health Events Analyzer     │  │ Fault Quarantine           │  │ Event Exporter             │
│ Filter + Process           │  │ Filter                     │  │ Process event              │
│ - Change-stream filter     │  │ - Change-stream filter     │  │ - Transform to CloudEvent  │
│   excludes STORE_ONLY      │  │   excludes STORE_ONLY      │  │ - Include                  │
│ - Rule queries exclude     │  │ - NO cordon/taints         │  │   processingStrategy       │
│   STORE_ONLY events        │  │ - NO labels/annotations    │  │ - Publish event            │
│ - If publishing as         │  │ - NO status update         │  │                            │
│   STORE_ONLY: publish      │  │                            │  │                            │
│   events with STORE_ONLY   │  │                            │  │                            │
└────────────────────────────┘  └────────────────────────────┘  └────────────────────────────┘
											↓
									Database NOT Updated
									(nodeQuarantined = null)
										   ↓
							┌──────────────┴──────────────┐
							↓                             ↓
					┌────────────────────────┐  ┌────────────────────────┐
					│ Node Drainer Module    │  │ Fault remediation      │
					│                        │  │ Module                 │
					│ will not receive event │  │ will not receive event │
					└────────────────────────┘  └────────────────────────┘
```
---

## Code Changes
### Step 1: Update Protobuf Definition

**File:** `data-models/protobufs/health_event.proto`

```protobuf
enum ProcessingStrategy {
  UNSPECIFIED = 0;
  EXECUTE_REMEDIATION = 1;
  STORE_ONLY = 2;
}

message HealthEvent {
  uint32 version = 1;
  string agent = 2;
  string componentClass = 3;
  string checkName = 4;
  bool isFatal = 5;
  bool isHealthy = 6;
  string message = 7;
  RecommendedAction recommendedAction = 8;
  repeated string errorCode = 9;
  repeated Entity entitiesImpacted = 10;
  map<string, string> metadata = 11;
  google.protobuf.Timestamp generatedTimestamp = 12;
  string nodeName = 13;
  BehaviourOverrides quarantineOverrides = 14;
  BehaviourOverrides drainOverrides = 15;
  
  // NEW: Client-requested processing strategy.
  // STORE_ONLY means the event is for observability only - no cluster resources should be modified.
  ProcessingStrategy processingStrategy = 16;
}
```
---

### Step 2: Health Monitor Implementation

**Syslog health monitor**

Health monitors read the `--processingStrategy` flag (or equivalent config) and pass it to **all handlers**. Each handler sets `processingStrategy` in health events before publishing.

**File:** `health-monitors/syslog-health-monitor/main.go`

```go
// Parse the flag/config
flag.Parse()
processingStrategy := *processingStrategyFlag // EXECUTE_REMEDIATION or STORE_ONLY

// Pass processingStrategy to ALL handler initializations
xidHandler := xid.NewXIDHandler(nodeName, agentName, componentClass, 
    checkName, xidAnalyserEndpoint, metadataPath, processingStrategy)

sxidHandler := sxid.NewSXIDHandler(nodeName, agentName, componentClass, 
    checkName, metadataPath, processingStrategy)

gpuFallenHandler := gpufallen.NewGPUFallenHandler(nodeName, agentName, 
    componentClass, checkName, processingStrategy)
```

Update each handler to accept and use `processingStrategy`:

File: `health-monitors/syslog-health-monitor/pkg/xid/xid_handler.go` (and similar for SXID, GPUFallen)

```go
type XIDHandler struct {
	// ... existing fields ...
	processingStrategy string // NEW: Store processingStrategy (EXECUTE_REMEDIATION or STORE_ONLY)
}

func NewXIDHandler(..., processingStrategy string) (*XIDHandler, error) {
	return &XIDHandler{
		// ... existing fields ...
		processingStrategy: processingStrategy,  // NEW: Store strategy
	}, nil
}

func (h *XIDHandler) createHealthEvent(xid int, message string) *pb.HealthEvent {
	event := &pb.HealthEvent{
		// ... all existing fields ...
		ProcessingStrategy: h.processingStrategy,  // NEW: Set strategy in event
	}
	return event
}
```

Apply to all handlers: XIDHandler, SXIDHandler, GPUFallenHandler (syslog-health-monitor)

**Kubernetes Object Monitor**

Kubernetes Object Monitor can set `processingStrategy` in two ways:
1. **Module level**: all events it publishes should have `processingStrategy=STORE_ONLY`.
2. **Rule level**: the monitor itself may run in standard mode, but a rule can be defined with `STORE_ONLY` strategy; any event published for a match on that rule should use `STORE_ONLY`.

Add an optional `processingStrategy` field to the policy health event config (TOML):

File: `health-monitors/kubernetes-object-monitor/pkg/config/types.go`

```toml
[[policies]]
name = "SomePolicy"
enabled = true

[policies.resource]
group = ""
version = "v1"
kind = "Node"

[policies.predicate]
expression = "true"

[policies.healthEvent]
componentClass = "GPU"
isFatal = false
message = "Example message"
recommendedAction = "NONE"
errorCode = ["0"]
processingStrategy = "STORE_ONLY" # optional; overrides module default for this policy
```

File: `health-monitors/kubernetes-object-monitor/pkg/publisher/publisher.go`

```go
type Publisher struct {
	pcClient pb.PlatformConnectorClient
	defaultProcessingStrategy string // NEW: Module-level default (EXECUTE_REMEDIATION or STORE_ONLY)
}

func New(client pb.PlatformConnectorClient, defaultProcessingStrategy string) *Publisher {
	return &Publisher{
		pcClient: client,
		defaultProcessingStrategy: defaultProcessingStrategy, // NEW: Store default strategy
	}
}

func (p *Publisher) PublishHealthEvent(ctx context.Context,
	policy *config.Policy, nodeName string, isHealthy bool) error {
	// Module-level default, with an optional rule-level override.
	strategy := p.defaultProcessingStrategy
	if policy.HealthEvent.ProcessingStrategy != "" {
		strategy = policy.HealthEvent.ProcessingStrategy
	}

	event := &pb.HealthEvent{
		// ... all existing fields ...
		ProcessingStrategy: strategy,
	}
	
	healthEvents := &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
	
	return p.sendWithRetry(ctx, healthEvents)
}
```

**CSP Health Monitor**
File: `health-monitors/csp-health-monitor/pkg/triggerengine/trigger.go`

```go
type Engine struct {
	store      datastore.Store
	udsClient  pb.PlatformConnectorClient
	config     *config.Config
	processingStrategy string // NEW: Store processing strategy (EXECUTE_REMEDIATION or STORE_ONLY)
	// ... other fields ...
}

func NewEngine(cfg *config.Config, store datastore.Store, 
	udsClient pb.PlatformConnectorClient, k8sClient kubernetes.Interface, 
	processingStrategy string) *Engine {
	return &Engine{
		// ... existing fields ...
		processingStrategy: processingStrategy,  // NEW: Store strategy
	}
}

func (e *Engine) mapMaintenanceEventToHealthEvent(
	event model.MaintenanceEvent, isHealthy, isFatal bool, message string,
) (*pb.HealthEvent, error) {
	healthEvent := &pb.HealthEvent{
		Agent:             "csp-health-monitor",
		// ... all existing fields ...
		ProcessingStrategy: e.processingStrategy,  // NEW: Set strategy in event
	}
	return healthEvent, nil
}
```

**GPU Health Monitor**

File: `health-monitors/gpu-health-monitor/gpu_health_monitor/cli.py`

```python
@click.command()
# ... existing options ...
@click.option("--processingStrategy", type=str, default="EXECUTE_REMEDIATION",
              help="Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY", required=False)
def cli(dcgm_addr, config_file, port, verbose, state_file, 
        dcgm_k8s_service_enabled, metadata_path, processingStrategy):
    # ... existing code ...
    
    # Pass processingStrategy to event processor
    event_processor = platform_connector.PlatformConnectorEventProcessor(
        socket_path=platform_connector_config["SocketPath"],
        node_name=node_name,
        # ... other params ...
        processing_strategy=processingStrategy  # NEW: Pass strategy
    )
```

File: `health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/platform_connector.py`

```python
class PlatformConnectorEventProcessor:
    def __init__(self, socket_path, node_name, exit, dcgm_errors_info_dict,
                 state_file_path, dcgm_health_conditions_categorization_mapping_config,
                 metadata_path, processing_strategy="EXECUTE_REMEDIATION"):  # NEW: Add parameter
        # ... existing fields ...
        self._processing_strategy = processing_strategy  # NEW: Store strategy
    
    def health_event_occurred(self, health_details, gpu_ids):
        # Creates HealthEvents in multiple places - add processingStrategy to ALL:
        
        health_event = platformconnector_pb2.HealthEvent(
            version=self._version,
            agent=self._agent,
            # ... all existing fields ...
            processingStrategy=self._processing_strategy,  # NEW: Add to ALL HealthEvent creations
        )
```

GPU Health Monitor creates HealthEvent objects in 4 different places in this file. Add `processingStrategy=self._processing_strategy` to all 4 locations.

### Step 3: Platform Connector (Kubernetes)

Platform connector receives health events through the gRPC connection:
- Store events in the database (MongoDB/PostgreSQL) for observability
- Create/update **node conditions** on Kubernetes nodes
- Create **Kubernetes events** for unhealthy, non-fatal events

For `processingStrategy=STORE_ONLY` events, the platform connector should:
1. Store events in database (needed for observability)
2. Not create node conditions
3. Not create Kubernetes events

**Normalization:** Platform connector normalizes `processingStrategy` in the gRPC server handler before any processing or enqueuing. If the field is missing or set to `UNSPECIFIED` (e.g., from custom monitors), it defaults to `EXECUTE_REMEDIATION`. This ensures all events in the database have an explicit strategy, providing consistent behavior across all modules.

File: `platform-connectors/pkg/server/platform_connector_server.go`

Add normalization in `HealthEventOccurredV1()` method before enqueuing to ring buffers:

```go
func (p *PlatformConnectorServer) HealthEventOccurredV1(ctx context.Context,
	he *pb.HealthEvents) (*empty.Empty, error) {
	slog.Info("Health events received", "events", he)

	healthEventsReceived.Add(float64(len(he.Events)))

	// Custom monitors that don't set processingStrategy will default to EXECUTE_REMEDIATION.
	for _, event := range he.Events {
		if event.ProcessingStrategy == pb.ProcessingStrategy_UNSPECIFIED {
			event.ProcessingStrategy = pb.ProcessingStrategy_EXECUTE_REMEDIATION
		}
	}

	// ... pipeline processing and enqueuing to ring buffers
}
```

File: `platform-connectors/pkg/connectors/kubernetes/process_node_events.go`

Add skip logic for `STORE_ONLY` events in `filterProcessableEvents()` method:

```go
func filterProcessableEvents(healthEvents *protos.HealthEvents) []*protos.HealthEvent {
	var processableEvents []*protos.HealthEvent
	for _, healthEvent := range healthEvents.Events {
		if healthEvent.ProcessingStrategy == protos.ProcessingStrategy_STORE_ONLY {
			slog.Info("Skipping STORE_ONLY event - no node conditions or K8s events will be created",
				"node", healthEvent.NodeName,
				"checkName", healthEvent.CheckName)
			continue  // Skip this event
		}
		processableEvents = append(processableEvents, healthEvent)
	}

	return processableEvents
}
```
---

### Step 4: Event Exporter
External systems need to know which `processingStrategy` was requested for proper monitoring.

File: `event-exporter/pkg/transformer/cloudevents.go`

Update `ToCloudEvent()` function to add a `processingStrategy` field so that from the metrics we can tell which events were published for observability only (`STORE_ONLY`):

```go
func ToCloudEvent(event *pb.HealthEvent, metadata map[string]string) (*CloudEvent, error) {
	// ... existing timestamp, entities code ...

	healthEventData := map[string]any{
		"version":            event.Version,
		"agent":              event.Agent,
		"componentClass":     event.ComponentClass,
		"checkName":          event.CheckName,
		"isFatal":            event.IsFatal,
		"isHealthy":          event.IsHealthy,
		"message":            event.Message,
		"recommendedAction":  event.RecommendedAction.String(),
		"errorCode":          errorCodes,
		"entitiesImpacted":   entities,
		"generatedTimestamp": timestamp,
		"nodeName":           event.NodeName,
		"processingStrategy": event.ProcessingStrategy.String(),  // NEW: Include processing strategy
	}

	// ... rest of method
}
```
---

### Step 5: Store Client 

We need new methods in mongodb and postgres pipeline builder:
1. `BuildProcessableHealthEventInsertsPipeline` which filters `processingStrategy=EXECUTE_REMEDIATION` inserted events for fault-quarantine module
2. `BuildProcessableNonFatalUnhealthyInsertsPipeline` which filters `processingStrategy=EXECUTE_REMEDIATION`, non-fatal and unhealthy inserted events for health-events-analyzer module.

File: `store-client/pkg/client/mongodb_pipeline_builder.go`

```go
// BuildProcessableHealthEventInsertsPipeline creates a pipeline that watches for
// all EXECUTE_REMEDIATION health event inserts.
//
// Backward Compatibility: This pipeline uses $or to match events where processingstrategy is either:
//   - EXECUTE_REMEDIATION (new events from NVSentinel health monitors)
//   - Missing/null (old events created before update, custom monitors, or circuit breaker backlog)
func (b *MongoDBPipelineBuilder) BuildProcessableHealthEventInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(
					datastore.E("$in", datastore.A("insert")),
				)),
				// Exclude STORE_ONLY events, but include EXECUTE_REMEDIATION and missing field
				datastore.E("$or", datastore.A(
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION))),
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", datastore.D(datastore.E("$exists", false)))),
				)),
			)),
		),
	)
}

// BuildProcessableNonFatalUnhealthyInsertsPipeline creates a pipeline for non-fatal, unhealthy event inserts
// excluding STORE_ONLY events. This is used by health-events-analyzer for pattern analysis.
//
// Backward Compatibility: Uses $or to include EXECUTE_REMEDIATION and missing field.
func (b *MongoDBPipelineBuilder) BuildProcessableNonFatalUnhealthyInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", "insert"),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
				// Exclude STORE_ONLY events, but include EXECUTE_REMEDIATION and missing field
				datastore.E("$or", datastore.A(
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION))),
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", datastore.D(datastore.E("$exists", false)))),
				)),
			)),
		),
	)
}
```

Same method required in postgres builder

File: `store-client/pkg/client/postgresql_pipeline_builder.go`

```go
// BuildProcessableHealthEventInsertsPipeline creates a pipeline that watches for health event inserts
// with processingStrategy=EXECUTE_REMEDIATION
//
// Backward Compatibility: Uses $or to include EXECUTE_REMEDIATION and missing field.
func (b *PostgreSQLPipelineBuilder) BuildProcessableHealthEventInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(
					datastore.E("$in", datastore.A("insert")),
				)),
				// Exclude STORE_ONLY events, but include EXECUTE_REMEDIATION and missing field
				datastore.E("$or", datastore.A(
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION))),
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", datastore.D(datastore.E("$exists", false)))),
				)),
			)),
		),
	)
}

// BuildProcessableNonFatalUnhealthyInsertsPipeline creates a pipeline for non-fatal, unhealthy event inserts
// excluding STORE_ONLY events. For PostgreSQL, handles both INSERT and UPDATE operations.
//
// Backward Compatibility: Uses $or to include EXECUTE_REMEDIATION and missing field.
func (b *PostgreSQLPipelineBuilder) BuildProcessableNonFatalUnhealthyInsertsPipeline() datastore.Pipeline {
	return datastore.ToPipeline(
		datastore.D(
			datastore.E("$match", datastore.D(
				datastore.E("operationType", datastore.D(datastore.E("$in", datastore.A("insert", "update")))),
				datastore.E("fullDocument.healthevent.agent", datastore.D(datastore.E("$ne", "health-events-analyzer"))),
				datastore.E("fullDocument.healthevent.ishealthy", false),
				// Exclude STORE_ONLY events, but include EXECUTE_REMEDIATION and missing field
				datastore.E("$or", datastore.A(
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", int32(protos.ProcessingStrategy_EXECUTE_REMEDIATION))),
					datastore.D(datastore.E("fullDocument.healthevent.processingstrategy", datastore.D(datastore.E("$exists", false)))),
				)),
			)),
		),
	)
}
``` 

---

### Step 5: Fault Quarantine Module

Update this module to skip processing `STORE_ONLY` events so that:
- Node is not cordoned
- Annotations and labels are not applied
- `nodeQuarantined` status is not set (remains null)

**Why skipping `nodeQuarantined` update matters:** fault-remediation and node-drainer module use pipeline filters that match on `nodeQuarantined` status. If this field is not set, their pipelines will exclude the event, and they won't see it.

File: `fault-quarantine/pkg/initializer/init.go`

Update the DB change stream pipeline to exclude `STORE_ONLY` events (so that fault-quarantine never receives them):

```go
func InitializeAll(ctx context.Context, params InitializationParams) (*Components, error) {
	// ...rest of the code
	builder := client.GetPipelineBuilder()
	
	// call new helper method which filters inserted events with processingStrategy=EXECUTE_REMEDIATION
	pipeline := builder.BuildProcessableHealthEventInsertsPipeline()

	// ...rest of the code
}
```


**Note:** We don't need skip logic in node-drainer and fault-remediation modules as they won't receive these events. Since fault-quarantine won't act on `STORE_ONLY` events, it won't set `nodeQuarantined`, so downstream pipeline filters will exclude them.

---

### Step 6: Health Events Analyzer

Health Events Analyzer has two distinct processingStrategy concerns:

1. **Incoming/historic events**: if an incoming (or historical) health event has `processingStrategy=STORE_ONLY`, it should be ignored
   by the analyzer for pattern detection (no queries/state transitions triggered by audit-only events).
2. **Analyzer’s own output strategy**: if the analyzer itself is configured to publish with `STORE_ONLY`, then any aggregated health events
   it publishes must have `processingStrategy=STORE_ONLY`.
3. **Rule-based output strategy**: a single rule can also be configured to publish with `STORE_ONLY`; any new event it publishes should also use `STORE_ONLY`.

**Skip processing STORE_ONLY events in events-analyzer**

Update the change-stream pipeline to exclude `processingStrategy=STORE_ONLY` events, so the analyzer does not receive observability-only events for pattern detection.

File: `health-events-analyzer/main.go`

```go
func createPipeline() interface{} {
	builder := client.GetPipelineBuilder()
	// use new helper method which filters events with processingStrategy=EXECUTE_REMEDIATION
	return builder.BuildProcessableNonFatalUnhealthyInsertsPipeline()
}
```

**Ignore STORE_ONLY events during mongo query**

Update the default pipeline query to exclude `processingStrategy=STORE_ONLY` events. We need this condition for every rule that's why we are adding it at code level instead of keeping it at config file level.

**Backward Compatibility Note:** Historical events in the database (created before this feature) won't have the `processingstrategy` field. These old events should be treated as `EXECUTE_REMEDIATION` (they were meant to be processed). We use `$or` to explicitly match both `EXECUTE_REMEDIATION` and a missing field to ensure backward compatibility. Other modules and health monitors do not require changes since they only act on newly inserted events, which will already have the `processingStrategy` field set (all health monitors run in `EXECUTE_REMEDIATION` mode by default). 

File: `health-events-analyzer/pkg/reconciler/reconciler.go`

```go
func (r *Reconciler) getPipelineStages(
	rule config.HealthEventsAnalyzerRule,
	healthEventWithStatus datamodels.HealthEventWithStatus,
) ([]map[string]interface{}, error) {
	// CRITICAL: Always start with agent filter to exclude events from health-events-analyzer itself
	// This prevents the analyzer from matching its own generated events, which would cause
	// infinite loops and incorrect rule evaluations
	//
	// Backward Compatibility: Use $or to include events where processingstrategy is either
	// EXECUTE_REMEDIATION or missing (old events created before this feature was added).
	// Old events without this field should be treated as EXECUTE_REMEDIATION.
	pipeline := []map[string]interface{}{
		{
			"$match": map[string]interface{}{
				"healthevent.agent": map[string]interface{}{"$ne": "health-events-analyzer"},
				"$or": []map[string]interface{}{
					{"healthevent.processingstrategy": "EXECUTE_REMEDIATION"},
					{"healthevent.processingstrategy": map[string]interface{}{"$exists": false}},
				},
			},
		},
	}
}
```

**health-events-analyzer publishing with STORE_ONLY (module or rule)**

Update the publish function to set the `processingStrategy` based on the module configuration and/or the rule configuration.

File: `health-events-analyzer/pkg/publisher/publisher.go`

```go
func (p *PublisherConfig) Publish(ctx context.Context, event *protos.HealthEvent,
	recommendedAction protos.RecommendedAction, ruleName string, message string, rule *config.HealthEventsAnalyzerRule) error {
	newEvent := proto.Clone(event).(*protos.HealthEvent)

	newEvent.Agent = "health-events-analyzer"
	newEvent.CheckName = ruleName
	newEvent.RecommendedAction = recommendedAction
	newEvent.IsHealthy = false
	newEvent.Message = message

	newEvent.ProcessingStrategy = p.processingStrategy // default
	if rule.processingStrategy != "" {
		newEvent.ProcessingStrategy = rule.processingStrategy
	}

	// ...rest of the code
}
```

---

### Step 7: Configuration Changes

Health monitors need to set `processingStrategy` based on configuration. The default strategy should be `EXECUTE_REMEDIATION` (standard mode where cluster operations may be performed downstream).

Add to Helm values.yaml (for each health monitor):

```yaml
syslogHealthMonitor:
  processingStrategy: STORE_ONLY  # EXECUTE_REMEDIATION or STORE_ONLY
cspHealthMonitor:
  processingStrategy: STORE_ONLY
kubernetesObjectMonitor:
  processingStrategy: STORE_ONLY
gpuHealthMonitor:
  processingStrategy: STORE_ONLY
```

Update deployment template (add to container args in `_helpers.tpl` or `deployment.yaml`):

```yaml
args:
  # ... existing args ...
  - "--processingStrategy={{ .Values.processingStrategy | default \"EXECUTE_REMEDIATION\" }}"
```

Add command-line flag in health monitor with default value set to `EXECUTE_REMEDIATION` (main.go):

```go
processingStrategyFlag = flag.String("processingStrategy", "EXECUTE_REMEDIATION", "Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY")
```

---

## Alternatives Considered

### Using Health Event Overrides (override isFatal=false)

We considered extending the existing health event override feature in platform-connector to support setting `isFatal=false` via CEL rules, instead of adding the flag at the health monitor level.

**Why this was rejected:**

1. Even when using health event overrides to mark events as non-fatal (`isFatal=false`), they still show up as Kubernetes node events. This creates confusion because:
   - Operators see node events and don't understand why they're present
   - NVBugs get open for node events
   - External systems running may alert on node/pod events

2. Client-side intent should be a property of the health monitors themselves. As new monitors are added, the health monitors should be able to clearly state (by setting `processingStrategy=STORE_ONLY`) that published events are for observability only and no module should take action on them.

### Using Health Event Overrides (override processingStrategy=STORE_ONLY)
We considered extending the existing health event override feature in platform-connector to support setting `processingStrategy=STORE_ONLY` via CEL rules, instead of setting it at the health monitor level.

**Why this was rejected:**
When we add a new health monitor or health check, we don't want to tell operators to both enable the monitor and also to configure overrides to force `processingStrategy=STORE_ONLY` (observability-only).
The decided approach is similar to how feature flags work, where the feature flags are set to the default recommended value and the operators can change it if required.

---