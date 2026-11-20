# Event Exporter Configuration

## Overview

The Event Exporter module exports health events from NVSentinel to external systems. It uses the CloudEvents format over HTTP. This document describes the main Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

This setting enables or disables the deployment of the Event Exporter in the cluster.

```yaml
global:
  eventExporter:
    enabled: true
```

> Note: This module works only with an enabled datastore. Thus, also enable the datastore.

### Logging

Sets the verbosity level of the Event Exporter logs.

```yaml
event-exporter:
  logLevel: info  # Options: debug, info, warn, error
```

### Resources

Defines the CPU and memory resource requests and limits for the Event Exporter pod.

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

The name of the Kubernetes secret that contains the OIDC client secret for authentication.

```yaml
event-exporter:
  oidcSecretName: "event-exporter-oidc-secret"
```

The secret must contain a key named `oidc-client-secret` with the client secret value. Before you deploy, create the secret:

```bash
kubectl create secret generic event-exporter-oidc-secret \
  --from-literal=oidc-client-secret='your-client-secret-here' \
  -n nvsentinel
```

## Metadata Configuration

Custom metadata fields that the exporter includes in all exported CloudEvents.

```yaml
event-exporter:
  exporter:
    metadata:
      cluster: "my-cluster"
      environment: "production"
```

The exporter puts the metadata fields in the CloudEvent `data.metadata` object. The `cluster` field is required. The exporter uses it to generate the CloudEvent `source` field.

### Custom Metadata Fields

Add any additional metadata fields:

```yaml
event-exporter:
  exporter:
    metadata:
      cluster: "prod-us-west-2"
      environment: "production"
      region: "us-west-2"
      datacenter: "dc01"
      tenant: "acme-corp"
```

The exporter includes all fields in exported events. Downstream systems can use them for filtering, routing, or categorization.

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

The HTTP/HTTPS URL to which the exporter POSTs CloudEvents.

#### timeout

The request timeout for HTTP calls to the sink endpoint.

#### insecureSkipVerify

Skips the TLS certificate verification. Set it to `true` only for tests with self-signed certificates.

## OIDC Authentication

The configuration for authentication with the OAuth 2.0 Client Credentials flow.

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

The URL of the OAuth 2.0 token endpoint that gives the access tokens.

#### clientId

The OAuth 2.0 client identifier.

#### scope

The OAuth 2.0 scope that the exporter requests for the access token.

#### insecureSkipVerify

Skips the TLS certificate verification for the token endpoint. Set it to `true` only for tests.

### Authentication Flow

The Event Exporter uses the OAuth 2.0 Client Credentials grant:

1. Requests an access token from `tokenUrl` with `clientId` and the client secret
2. Caches the token until it expires
3. Includes the token in the `Authorization: Bearer {token}` header of event POSTs
4. Automatically refreshes expired tokens

## HTTP Proxy

Sink and OIDC token requests use the standard `HTTPS_PROXY`, `HTTP_PROXY`, and `NO_PROXY` environment variables. To set them on the Event Exporter container, use `extraEnv`:

```yaml
event-exporter:
  extraEnv:
    - name: HTTPS_PROXY
      value: "http://proxy.example.com:3128"
    - name: NO_PROXY
      value: ".svc,.cluster.local"
```

`extraEnv` accepts any Kubernetes `EnvVar` entries. These include `valueFrom` references to a Secret, for a proxy URL that contains credentials. The default is an empty list.

Datastore connections (MongoDB or PostgreSQL) do not use these variables.

## Backfill Configuration

Controls whether the exporter exports historical events when it starts.

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

Enables the backfill of historical events from the datastore.

#### maxAge

The maximum age of events to backfill (for example, "720h" = 30 days).

#### maxEvents

The maximum number of historical events to process during backfill.

#### batchSize

The number of events to process in each batch during backfill.

#### rateLimit

The maximum number of events per second to export during backfill. This limit prevents an overload of the sink.

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

## Resume Token Storage

The location where the exporter persists its change stream resume token. After a restart, the exporter continues from the last exported event. It does not replay the stream.

```yaml
event-exporter:
  exporter:
    resumeToken:
      database: "nvsentinel"
      collection: "resumetokens"
```

Both fields are required. If one of them is empty, the exporter rejects its configuration at startup. If your datastore uses different names, change these fields. Otherwise, keep the defaults.

The token advances in strict event order, independent of which worker publishes first. Thus, if you increase [Workers](#workers), there is no risk of skipped events after a restart.

## Workers

The number of concurrent workers that process events and publish them to the sink in parallel.

```yaml
event-exporter:
  exporter:
    workers: 10
```

Each worker takes events independently, processes them (unmarshal, transform, publish), and reports the result. The exporter advances resume tokens in strict order, independent of which worker finishes first. Thus, more workers increase throughput and keep the at-least-once delivery guarantees. With concurrent publishing, events can arrive at the sink out of order.

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

If your publish latency is lower (for example, 100ms for a co-located endpoint), each worker handles proportionally more events. To estimate the throughput of one worker, multiply 3.3 events/sec by 300ms divided by your publish latency.

## Event Filter

Selects which events reach the sink. An empty expression exports all events. This is the default.

```yaml
event-exporter:
  exporter:
    filter:
      expression: "event.recommendedAction != 'NONE' && !('45' in event.errorCode)"
```

### Parameters

#### expression

A CEL expression over the health event. It must evaluate to a boolean. If the expression is false for an event, the exporter does not publish that event.

### When to use it

If you want to drop events before they reach the sink, use the filter. This is common when the sink sends notifications and does not archive all events. Most events recommend no action, so a notification about them is not useful.

By default, the exporter exports all events. For a data lake, filter the events at the destination.

### Available fields

The expression uses the same vocabulary as the [override transformer](platform-connectors.md#override-transformer-configuration) of the platform connector. Thus, an expression that you write for one also works in the other:

| Field | Type |
| --- | --- |
| `event.agent` | string |
| `event.checkName` | string |
| `event.componentClass` | string |
| `event.errorCode` | **list of string** |
| `event.isFatal` | bool |
| `event.isHealthy` | bool |
| `event.recommendedAction` | string, the enum name such as `NONE` or `CONTACT_SUPPORT` |
| `event.customRecommendedAction` | string, the custom action name as sent. Also check `event.recommendedAction == 'CUSTOM'`, because an event with a built-in action can also have a name |
| `event.nodeName` | string |
| `event.metadata` | map of string to string |
| `event.message` | string |

> **`errorCode` is a list, not a string.** A health event can have several codes. Thus, use a membership match: `'45' in event.errorCode`, not `event.errorCode == '45'`. The second form causes a type error, not a false match.

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

**Validated at startup.** The exporter compiles the expression during config validation. Thus, if the expression has a syntax error or is not boolean, the exporter does not start. The error does not occur later for each event.

> **Write a comparison, not a bare field.** The exporter binds `event` as a dynamically typed map. Thus, a bare read has no type, and the exporter rejects it, *including* a semantically boolean one. Use `event.isFatal == true`, not `event.isFatal`.
>
> For the boolean fields, this rule is stricter than necessary. But it lets the exporter catch `event.agent` at startup instead of on the first event. The error message names the working form.

**It fails open.** If an evaluation error occurs, the exporter exports the event and increments `health_events_exporter_filter_errors_total`. A drop of events because of a filter bug is silent data loss. An extra exported event is only noise, and the sink already tolerates noise. Create an alert for when that counter is not zero.

**Filtered events still advance the resume token.** The exporter completes a filtered event and does not skip it, so the stream makes progress. This is important. A skipped event at the head of the stream stops the resume token, and each restart then delivers all later events again. With a filter that drops 99% of events, skipped events can stop all progress of the stream. The exporter completes filtered events, so this problem does not occur.

**It applies to backfill too**, so a backfill run exports the same subset as the live stream.

`health_events_exporter_events_filtered_total` counts the events that the filter dropped.

## Failure Handling

Configures the retry behavior for failed export attempts.

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

The maximum number of retry attempts for a failed export. After this number, the exporter stops the retries.

#### initialBackoff

The initial delay before the first retry attempt.

#### maxBackoff

The maximum delay between retry attempts. This value limits the exponential backoff.

#### backoffMultiplier

The multiplier for the exponential backoff calculation.

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
3. Validate the `Authorization: Bearer {token}` header. Verify the token authenticity and validity with your auth provider (the example shows only the expected header format)
4. Return HTTP 2xx status codes for successful ingestion
5. Return HTTP 4xx/5xx status codes for failures
6. Handle CloudEvents 1.0 JSON format

### Example Sink Implementation

> **Note**: The code below is illustrative pseudocode that shows how to handle the request structure. The `# Verify Bearer token` section checks only the header format. Replace it with real token signature verification and validation with your authentication provider. For example, use JWT verification, OAuth introspection, or a shared-secret HMAC check.

This example shows the steps of a minimal sink endpoint:

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
