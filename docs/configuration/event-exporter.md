# Event Exporter Configuration

## Overview

The Event Exporter module exports health events from NVSentinel to external systems using CloudEvents format over HTTP. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the event-exporter module is deployed in the cluster.

```yaml
global:
  eventExporter:
    enabled: true
```

> Note: This module depends on the datastore being enabled. Therefore, ensure the datastore is also enabled.

### Resources

Defines CPU and memory resource requests and limits for the event-exporter pod.

```yaml
event-exporter:
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "500m"
      memory: "512Mi"
```

### OIDC Secret

Name of the Kubernetes secret containing OIDC client secret for authentication.

```yaml
event-exporter:
  oidcSecretName: "event-exporter-oidc-secret"
```

The secret must contain a key named `oidc-client-secret` with the client secret value. Create the secret before deploying:

```bash
kubectl create secret generic event-exporter-oidc-secret \
  --from-literal=oidc-client-secret='your-client-secret-here' \
  -n nvsentinel
```

## Metadata Configuration

Custom metadata fields included in all exported CloudEvents.

```yaml
event-exporter:
  exporter:
    metadata:
      cluster: "my-cluster"
      environment: "production"
```

Metadata fields are included in the CloudEvent `data.metadata` object. The `cluster` field is required and used to generate the CloudEvent `source` field.

### Custom Metadata Fields

Add any additional metadata fields:

```yaml
event-exporter:
  metadata:
    cluster: "prod-us-west-2"
    environment: "production"
    region: "us-west-2"
    datacenter: "dc01"
    tenant: "acme-corp"
```

All fields are included in exported events and can be used for filtering, routing, or categorization in downstream systems.

## Sink Configuration

Defines the destination endpoint for exported events.

```yaml
event-exporter:
  exporter:
    sink:
      endpoint: "https://events.example.com/api/v1/events"
      timeout: "30s"
      insecureSkipVerify: false
```

### Parameters

#### endpoint
HTTP/HTTPS URL where CloudEvents will be POSTed.

#### timeout
Request timeout for HTTP calls to the sink endpoint.

#### insecureSkipVerify
Skip TLS certificate verification. Set to `true` only for testing with self-signed certificates.

## OIDC Authentication

Configuration for OAuth 2.0 Client Credentials flow authentication.

```yaml
event-exporter:
  exporter:
    oidc:
      tokenUrl: "https://auth.example.com/oauth2/token"
      clientId: "nvsentinel-exporter"
      scope: "events:write"
      insecureSkipVerify: false
```

### Parameters

#### tokenUrl
OAuth 2.0 token endpoint URL for obtaining access tokens.

#### clientId
OAuth 2.0 client identifier.

#### scope
OAuth 2.0 scope requested for access token.

#### insecureSkipVerify
Skip TLS certificate verification for token endpoint. Set to `true` only for testing.

### Authentication Flow

The event exporter uses OAuth 2.0 Client Credentials grant:

1. Requests access token from `tokenUrl` using `clientId` and client secret
2. Caches the token until expiration
3. Includes token in `Authorization: Bearer {token}` header for event POSTs
4. Automatically refreshes expired tokens

## Backfill Configuration

Controls whether historical events are exported when the exporter starts.

```yaml
event-exporter:
  exporter:
    backfill:
      enabled: true
      maxAge: "720h"
      maxEvents: 1000000
      batchSize: 500
      rateLimit: 1000
```

### Parameters

#### enabled
Enable backfilling of historical events from the datastore.

#### maxAge
Maximum age of events to backfill (e.g., "720h" = 30 days).

#### maxEvents
Maximum number of historical events to process during backfill.

#### batchSize
Number of events to process in each batch during backfill.

#### rateLimit
Maximum events per second to export during backfill to avoid overwhelming the sink.

### Backfill Examples

#### Conservative Backfill

```yaml
backfill:
  enabled: true
  maxAge: "168h"      # 7 days
  maxEvents: 10000
  batchSize: 100
  rateLimit: 100
```

#### Aggressive Backfill

```yaml
backfill:
  enabled: true
  maxAge: "2160h"     # 90 days
  maxEvents: 5000000
  batchSize: 1000
  rateLimit: 5000
```

#### Disabled Backfill

```yaml
backfill:
  enabled: false
```

## Workers

Number of concurrent goroutines that process and publish events to the sink in parallel.

```yaml
event-exporter:
  exporter:
    workers: 10
```

Each worker independently picks events from the dispatch queue, processes them (unmarshal, transform, publish), and reports the result. A sequence tracker ensures resume tokens advance in strict order regardless of which worker finishes first, so increasing workers scales throughput while preserving at-least-once delivery guarantees. Note that concurrent publishing means events may arrive at the sink out of order.

The default of `10` handles clusters up to ~3,300 nodes at typical event rates.

### Scale-Up Guide

**Event production rate**: ~10 events/sec per 1,000 nodes (~36,000 events/hour)
**Per-worker throughput**: ~3.3 events/sec (at 300ms publish latency)

| Workers | Throughput (events/sec) | Max Nodes Supported |
|---------|-------------------------|---------------------|
| 1       | 3.3                     | ~330                |
| 2       | 6.6                     | ~660                |
| 3       | 9.9                     | ~990                |
| 5       | 16.5                    | ~1,650              |
| 10      | 33                      | ~3,300              |
| 15      | 49.5                    | ~5,000              |
| 20      | 66                      | ~6,600              |

If your publish latency is lower (e.g., 100ms for a co-located endpoint), each worker handles proportionally more events — divide the latency ratio to estimate your actual throughput.

## Event Filter

Selects which events reach the sink. An empty expression exports everything, which is the default and matches previous behaviour.

```yaml
event-exporter:
  exporter:
    filter:
      expression: "event.recommendedAction != 'NONE' && !('45' in event.errorCode)"
```

### Parameters

#### expression
CEL over the health event. Must evaluate to a boolean; an event whose expression is false is not published.

### When to use it

Use it when you want to drop events before they reach the sink. This is common when the sink sends out notifications rather than archiving everything, since most events recommend no action and are not worth notifying on.

Exporting everything to a data lake and filtering at the destination remains the default.

### Available fields

The expression is evaluated against the vocabulary in `commons/pkg/celevent`, shared with the platform connector's [override transformer](platform-connectors.md#override-transformer-configuration), so an expression learned for one works in the other:

| Field | Type |
| --- | --- |
| `event.agent` | string |
| `event.checkName` | string |
| `event.componentClass` | string |
| `event.errorCode` | **list of string** |
| `event.isFatal` | bool |
| `event.isHealthy` | bool |
| `event.recommendedAction` | string, the enum name such as `NONE` or `CONTACT_SUPPORT` |
| `event.nodeName` | string |
| `event.metadata` | map of string to string |
| `event.message` | string |

> **`errorCode` is a list, not a string.** A health event can carry several codes, so match with membership: `'45' in event.errorCode`, not `event.errorCode == '45'`. The latter is a type error, not a false match.

### Examples

```yaml
# Actionable events only. Drops the ~99% that recommend no action.
expression: "event.recommendedAction != 'NONE'"

# Actionable, but exclude a known-noisy code.
expression: "event.recommendedAction != 'NONE' && !('45' in event.errorCode)"

# One agent's fatal events, plus their recoveries.
expression: "event.agent == 'gpu-health-monitor' && (event.isFatal == true || event.isHealthy == true)"

# Everything except a specific check.
expression: "event.checkName != 'GpuPowerWatch'"
```

### Operational notes

**Validated at startup.** The expression is compiled during config validation, so a syntax error or a non-boolean expression stops the exporter from starting rather than failing per event.

> **Write a comparison, not a bare field.** `event` is bound as a dynamically typed map, so a bare read is untyped and rejected, *including* a semantically boolean one. Use `event.isFatal == true`, not `event.isFatal`. This is stricter than strictly necessary for the boolean fields, and it is the trade that lets `event.agent` be caught at startup instead of on the first event. The error message names the working form.

**It fails open.** An evaluation error exports the event and increments `health_events_exporter_filter_errors_total`. Dropping events because of a filter bug is silent data loss, whereas exporting an extra event is noise the sink already tolerates. Alert on that counter being non-zero.

**Filtered events still advance the resume token.** A filtered event is completed rather than skipped, so the stream makes progress. This matters: were it skipped, one filtered event at the head of the stream would stall the token and a restart would redeliver everything after it, which with a filter dropping 99% of events would mean never making progress.

**It applies to backfill too**, so a backfill run exports the same subset as the live stream.

`health_events_exporter_events_filtered_total` counts events the filter dropped.

## Failure Handling

Configures retry behavior for failed export attempts.

```yaml
event-exporter:
  exporter:
    failureHandling:
      maxRetries: 17
      initialBackoff: "1s"
      maxBackoff: "5m"
      backoffMultiplier: 2.0
```

### Parameters

#### maxRetries
Maximum number of retry attempts for failed exports before giving up.

#### initialBackoff
Initial delay before first retry attempt.

#### maxBackoff
Maximum delay between retry attempts (caps exponential backoff).

#### backoffMultiplier
Multiplier for exponential backoff calculation.

### Retry Examples

#### Fast Retries

```yaml
failureHandling:
  maxRetries: 10
  initialBackoff: "100ms"
  maxBackoff: "10s"
  backoffMultiplier: 1.5
```

#### Conservative Retries

```yaml
failureHandling:
  maxRetries: 30
  initialBackoff: "5s"
  maxBackoff: "15m"
  backoffMultiplier: 2.5
```

## Sink Endpoint Requirements

The external event sink must:

1. Accept `POST` requests at the configured endpoint
2. Accept `Content-Type: application/cloudevents+json` header
3. Validate `Authorization: Bearer {token}` header (verify token authenticity and validity using your auth provider — the example shows the expected header format only)
4. Return HTTP 2xx status codes for successful ingestion
5. Return HTTP 4xx/5xx status codes for failures
6. Handle CloudEvents 1.0 JSON format

### Example Sink Implementation

> **Note**: The following is illustrative pseudocode showing request structure handling. The `# Verify Bearer token` section only checks the header format — replace it with actual token signature verification and validation using your authentication provider (e.g. JWT verification, OAuth introspection, or a shared-secret HMAC check).

A minimal sink endpoint should:

```python
@app.route('/api/v1/events', methods=['POST'])
def receive_event():
    # Verify Bearer token
    auth_header = request.headers.get('Authorization')
    if not auth_header or not auth_header.startswith('Bearer '):
        return {'error': 'Unauthorized'}, 401
    
    # Verify Content-Type
    if request.content_type != 'application/cloudevents+json':
        return {'error': 'Unsupported Media Type'}, 415
    
    # Parse CloudEvent
    event = request.json
    if event.get('specversion') != '1.0':
        return {'error': 'Unsupported CloudEvents version'}, 400
    
    # Process event
    process_health_event(event['data']['healthEvent'])
    
    return {'status': 'accepted'}, 202
```
