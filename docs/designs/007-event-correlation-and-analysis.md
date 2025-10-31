# ADR-007: Intelligence — Health Event Correlation

## Context

Many hardware issues manifest initially as non-fatal events that become critical when repeated. For example:

- **XID 13** (Graphics Engine Error): A single occurrence may be a transient application issue. Multiple occurrences on the same GPU indicate a hardware problem requiring investigation.
- **XID 31** (Fifo MMU Error): Similar pattern - one-time events are noise, repeated events signal degradation.
- **Thermal throttling**: Occasional thermal events during peak load are normal. Persistent throttling indicates cooling problems.

Currently, these non-fatal events are published as Kubernetes Events, which get overlooked because they're transient and considered application-level issues. The system needs to:
1. Track occurrences of non-fatal events over time
2. Correlate events by entity (specific GPU), not just node
3. Elevate non-fatal events to fatal when patterns match known failure modes
4. Make rules configurable so they can be updated as new patterns emerge

Options considered:
1. Client-side correlation in each module
2. Stateful event processing in a dedicated analyzer
3. Database-native correlation using aggregation pipelines

## Decision

Implement a Health Events Analyzer that uses MongoDB aggregation pipelines to correlate non-fatal events and determine when they should be treated as fatal. The analyzer is driven by configurable rules that define correlation logic.

## Implementation

The Health Events Analyzer is a controller that:
- Watches MongoDB for health events
- For each event, evaluates configured aggregation rules
- If rules match, publishes a new fatal event to trigger remediation

Architecture:
```
Health Event
      |
      v
Health Events Analyzer
      |
      +-- Load Rules from ConfigMap
      |
      +-- Execute MongoDB Aggregation Pipeline
      |
      +-- Check Result
      |
      v
If Match: Publish Fatal Event to UDS
```

Example rule configuration:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: health-events-analyzer-rules
data:
  rules.yaml: |
    - name: "XID13 Multiple Occurrences"
      description: "XID 13 occurring 3+ times on same GPU in 1 hour"
      checkInterval: 60s
      aggregationPipeline: |
        [
          {
            "$match": {
              "events.entitiesimpacted.entitytype": "GPU",
              "events.errorcode": "13",
              "events.generatedat": {
                "$gte": {"$date": "{{.OneHourAgo}}"}
              }
            }
          },
          {
            "$group": {
              "_id": "$events.entitiesimpacted.entityvalue",
              "count": {"$sum": 1}
            }
          },
          {
            "$match": {
              "count": {"$gte": 3}
            }
          }
        ]
      fatalEvent:
        componentClass: "GPU"
        checkName: "CorrelatedXID13"
        message: "XID 13 occurred {{.Count}} times on GPU {{.GPUUUID}} in the last hour"
        recommendedAction: "NODE_REBOOT"
        errorCode: "XID13_REPEATED"
    
    - name: "XID31 Multiple Occurrences"
      description: "XID 31 occurring 3+ times on same GPU in 1 hour"
      checkInterval: 60s
      aggregationPipeline: |
        [
          {
            "$match": {
              "events.entitiesimpacted.entitytype": "GPU",
              "events.errorcode": "31",
              "events.generatedat": {
                "$gte": {"$date": "{{.OneHourAgo}}"}
              }
            }
          },
          {
            "$group": {
              "_id": "$events.entitiesimpacted.entityvalue",
              "count": {"$sum": 1}
            }
          },
          {
            "$match": {
              "count": {"$gte": 3}
            }
          }
        ]
      fatalEvent:
        componentClass: "GPU"
        checkName: "CorrelatedXID31"
        message: "XID 31 occurred multiple times"
        recommendedAction: "REPORT_ISSUE"
        errorCode: "XID31_REPEATED"
```

Execution flow:
1. Analyzer runs each rule on its checkInterval
2. Pipeline queries MongoDB for matching events
3. If pipeline returns results, a fatal event is generated
4. Fatal event includes context from aggregation (count, GPU UUID)
5. Event is published via UDS to platform connectors
6. Quarantine/drainer/remediation modules react to the fatal event

## Rationale

- **Database-native**: Leverages MongoDB's aggregation framework instead of loading events into memory
- **Flexible**: Aggregation pipelines support complex queries (time windows, grouping, counting)
- **Configurable**: Rules can be updated via ConfigMap without code changes
- **Efficient**: Aggregation runs server-side with indexes
- **Expressive**: Templates allow dynamic time ranges and thresholds
- **Scalable**: Offloads computation to MongoDB cluster

## Consequences

### Positive
- Detects patterns that simple thresholds miss
- Reduces false positives (single transient errors don't trigger remediation)
- Rules can be tuned per-cluster based on operational experience
- Historical analysis is possible (how many times has this pattern occurred?)
- No need to maintain state in the analyzer - MongoDB is the source of truth
- New correlation patterns can be added by operators

### Negative
- Requires operators to understand MongoDB aggregation syntax
- Aggregation queries add load to MongoDB
- Time windows require careful tuning (too short = miss patterns, too long = delayed response)
- Rules are evaluated periodically, not in real-time
- Complex pipelines can be slow on large event datasets

### Mitigations
- Provide rule templates for common correlation patterns
- Add indexes to MongoDB on frequently queried fields (errorCode, timestamp, GPU UUID)
- Document best practices for pipeline performance
- Include validation for aggregation syntax in ConfigMap admission
- Monitor pipeline execution time and alert on slowness
- Consider implementing incremental aggregation for better performance
- Set appropriate TTLs on health events to limit dataset size

## Alternatives Considered

### Client-Side Correlation in Each Module
**Rejected** because: Each module would need to maintain state and query logic. This duplicates code across quarantine, drainer, and remediation modules. State management becomes complex (what happens on pod restart?). No single source of truth for which events triggered which actions.

### Stateful Stream Processing (Flink, Spark Streaming)
**Rejected** because: Adds significant operational complexity with external dependencies. Requires deploying and managing a stream processing cluster. Overkill for the relatively simple correlation patterns needed. Event volume (typically <100 events/hour per node) doesn't justify stream processing overhead.

### Rules Engine (Drools, etc.)
**Rejected** because: Introduces new technology stack and programming model. Rules engines are designed for complex business logic, not time-series event correlation. MongoDB aggregation is more natural fit for database queries. Additional dependency increases operational burden.

### Hardcoded Correlation Logic
**Rejected** because: New patterns require code changes and deployments. Different clusters may need different thresholds. As new GPU generations emerge with different failure modes, hardcoded logic becomes a bottleneck. Configuration-driven approach is more maintainable.

## Notes

- Aggregation pipelines should be tested thoroughly before production deployment
- Consider adding a dry-run mode that logs what would be published without actually publishing
- Rules should be versioned and tracked in source control
- Include metrics on rule evaluations (count, execution time, matches)
- Future enhancement: machine learning to auto-detect patterns
- Event retention policies should align with longest correlation time window

## References

- MongoDB Aggregation: https://www.mongodb.com/docs/manual/core/aggregation-pipeline/
