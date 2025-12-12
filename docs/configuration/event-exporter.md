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
3. Includes token in `Authorization: Bearer <token>` header for event POSTs
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
3. Validate `Authorization: Bearer <token>` header
4. Return HTTP 2xx status codes for successful ingestion
5. Return HTTP 4xx/5xx status codes for failures
6. Handle CloudEvents 1.0 JSON format

### Example Sink Implementation

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
