# ADR-008: Integration — Cloud Provider Maintenance Events

## Context

Cloud service providers schedule maintenance on infrastructure that affects GPU nodes. These maintenance events need to be detected and handled by the node resilience system to trigger quarantine and remediation workflows before CSP-initiated disruptions occur.

Each CSP exposes maintenance information differently:
- **GCP**: Writes maintenance events to Cloud Logging
- **AWS**: Provides Health API for scheduled EC2 maintenance events

The system needs to:
1. Poll CSP APIs/logs for maintenance events
2. Store events in MongoDB
3. Trigger health events to NVSentinel platform connectors at appropriate times
4. Track event status through their lifecycle (detected, ongoing, complete)

## Decision

Implement a CSP Health Monitor with separate client implementations for GCP and AWS that poll for maintenance events, normalize them to a standard format, store them in MongoDB, and run a trigger engine that generates health events to NVSentinel based on timing thresholds.

## Implementation

Architecture has three main components:

### 1. CSP-Specific Clients (Polling)

**GCP Client:**
- Polls GCP Cloud Logging API (not instance metadata)
- Uses log filter to find maintenance-related log entries
- Queries by timestamp range to get new events since last poll
- Maps GCP instance IDs to Kubernetes node names via node provider IDs
- Polls at configurable interval (default: API polling interval from config)

**AWS Client:**
- Polls AWS Health API for EC2 scheduled maintenance events
- Filters by service (EC2), region, and event type categories
- Maps EC2 instance IDs to Kubernetes node names
- Supports event types: instance-stop, instance-reboot, system-reboot, system-maintenance

### 2. Event Processor (Normalization & Storage)

Receives raw CSP events and:
- Normalizes to standard `MaintenanceEvent` model
- Ensures cluster name is set
- Applies state inheritance (PENDING → ONGOING → COMPLETE transitions)
- Stores events in MongoDB collection `maintenance_events`
- Each event has: EventID, NodeName, MaintenanceType, Status, timestamps

Event statuses:
- `DETECTED` - Initial state when maintenance is scheduled
- `ONGOING` - Maintenance window has started
- `COMPLETE` - Maintenance finished
- `QUARANTINE_TRIGGERED` - Quarantine workflow initiated
- `HEALTHY_TRIGGERED` - Healthy signal sent after completion

### 3. Trigger Engine (Health Event Generation)

Runs in separate container (`maintenance-notifier`):
- Polls MongoDB for events needing action
- Generates health events to NVSentinel via UDS at appropriate times
- Two types of triggers:
  
**Quarantine Trigger:**
```
Query: Events in DETECTED status where 
       scheduledStartTime - now < triggerTimeLimitMinutes

Action: Send health event with isFatal=true to platform connector
Updates event status to QUARANTINE_TRIGGERED
```

**Healthy Trigger:**
```
Query: Events in COMPLETE status where
       actualEndTime + healthyDelayMinutes < now
       AND status != HEALTHY_TRIGGERED

Action: Send health event with isHealthy=true to platform connector
Updates event status to HEALTHY_TRIGGERED
```

**Node Readiness Monitor:**
- Watches quarantined nodes to see when they become ready again
- Sends healthy event if node becomes ready before maintenance starts
- Prevents unnecessary quarantine if CSP cancels maintenance

Deployment:
- Main container runs CSP client + event processor
- Sidecar container runs trigger engine
- Both share MongoDB connection
- UDS connection to platform-connectors on same node

Configuration (TOML):
```toml
clusterName = "my-cluster"

[gcp]
enabled = true
targetProjectID = "my-project"
apiPollingIntervalSeconds = 300  # 5 minutes
logFilter = "resource.type=\"gce_instance\" AND jsonPayload.event_subtype=\"compute.instances.hostMaintenance.migrate\""

[aws]
enabled = false
accountID = "123456789"
region = "us-west-2"
apiPollingIntervalSeconds = 300

maintenanceEventPollIntervalSeconds = 60  # Trigger engine poll interval
triggerQuarantineWorkflowTimeLimitMinutes = 10
postMaintenanceHealthyDelayMinutes = 5
```

## Rationale

- **Proactive**: Quarantine nodes before CSP-initiated maintenance disrupts workloads
- **Separation of concerns**: Polling/storage separate from trigger logic
- **State tracking**: MongoDB provides full event lifecycle history
- **Timing control**: Configurable thresholds for when to trigger quarantine
- **Post-maintenance recovery**: Automatic healthy signal after maintenance completes
- **Prevents false positives**: Node readiness monitor handles CSP-cancelled events

## Consequences

### Positive
- Advance warning enables graceful workload migration
- Complete event lifecycle tracking in MongoDB
- Trigger timing is configurable per-cluster
- Node readiness monitoring prevents unnecessary quarantine
- Post-maintenance healthy signal automates recovery
- Decoupled architecture (polling, storage, triggering)

### Negative
- Requires MongoDB for state management
- Polling introduces slight delay in event detection
- Two containers needed (main + trigger sidecar)
- Trigger timing must be tuned per environment
- Limited to CSPs that provide maintenance APIs (GCP Logging, AWS Health)

### Mitigations
- MongoDB is already required by NVSentinel
- Poll intervals are configurable
- Sidecar pattern is standard Kubernetes practice
- Provide recommended trigger timing configurations
- Document CSP API requirements and limitations

## Alternatives Considered

### Direct UDS Publishing from CSP Client
**Rejected** because: Would bypass event lifecycle tracking. No way to prevent duplicate triggers for same event. Can't handle state transitions (PENDING → ONGOING → COMPLETE). MongoDB provides necessary state management and deduplication.

### Single Container (No Trigger Sidecar)
**Rejected** because: Would tightly couple polling and triggering logic. Different poll intervals needed (CSP API vs MongoDB query). Separation allows independent scaling and retry logic for each concern.

### Metadata Service APIs Instead of Cloud APIs
**Rejected** for GCP because: GCP doesn't expose maintenance info via instance metadata. Cloud Logging is the authoritative source. For AWS, Health API provides more complete event info than instance metadata.

### Webhook-Based CSP Integration
**Rejected** because: CSPs don't support webhooks for maintenance events. Would require exposing cluster endpoints to CSP, creating security concerns. Polling is standard pattern supported by all CSPs.

## Notes

- MongoDB collection `maintenance_events` stores full event history
- Trigger engine runs independently of CSP polling
- Node readiness check prevents false positives from cancelled maintenance
- GCP uses Cloud Logging API, not instance metadata
- AWS uses Health API with EC2 event filtering
- Event IDs from CSP are used to prevent duplicate processing
- State transitions are one-way (DETECTED → ONGOING → COMPLETE)

## References

- Related: ADR-001 (Health Event Detection Interface)
- Related: ADR-002 (Storage Layer Selection)
- GCP Cloud Logging: https://cloud.google.com/logging/docs
- AWS Health API: https://docs.aws.amazon.com/health/latest/ug/what-is-aws-health.html
