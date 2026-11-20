# Event Exporter

## Overview

The Event Exporter streams health events from the NVSentinel datastore to external systems. It transforms them into the CloudEvents format for integration with enterprise monitoring, analytics, and alerting platforms.

The Event Exporter takes the health events that NVSentinel generates and delivers them to your external systems. Use it for centralized visibility, long-term storage, or integration with existing incident management workflows.

### Why Do You Need This?

NVSentinel handles automated remediation in the cluster. But you often also need to do these tasks:

- **Centralized monitoring**: Aggregate events from multiple clusters into one view
- **Long-term analytics**: Store events in data warehouses for trend analysis and reporting
- **Integration**: Send events to existing incident management, ticketing, or alerting systems
- **Compliance**: Meet audit and compliance requirements for event logging
- **Multi-cluster visibility**: Monitor GPU health across all of your infrastructure

The Event Exporter supports these use cases. It streams events to your external systems in real time, in the industry-standard CloudEvents format.

## How It Works

The Event Exporter runs as a deployment in the cluster. It does these steps:

1. Watches the datastore for new health events with change streams
2. Backfills historical events from the past N days, if backfill is enabled. It does this when it starts without a resume token, for example on the first startup
3. Transforms health events into the CloudEvents format with custom metadata
4. Publishes events to the configured HTTP endpoint with OIDC authentication
5. Tracks progress with resume tokens for reliable delivery
6. Retries failed publishes with exponential backoff

The exporter persists resume tokens, so delivery is at least once. After a restart, the exporter can publish some events again. On MongoDB, it can also skip events if its resume token becomes too old for the oplog. For more information, see [Resume Token Storage](configuration/event-exporter.md#resume-token-storage).

## Configuration

Configure the Event Exporter through Helm values:

```yaml
global:
  eventExporter:
    enabled: true

event-exporter:
  # OIDC secret (must be created manually)
  oidcSecretName: "event-exporter-oidc-secret"
  
  exporter:
    # Metadata included with every event
    metadata:
      cluster: "production-us-west"
      environment: "production"
      region: "us-west-2"
    
    # Destination endpoint
    sink:
      endpoint: "https://events.example.com/api/v1/events"
      timeout: "30s"
      insecureSkipVerify: false
    
    # OIDC authentication
    oidc:
      tokenUrl: "https://auth.example.com/oauth2/token"
      clientId: "nvsentinel-exporter"
      scope: "events:write"
      insecureSkipVerify: false
    
    # Historical event backfill
    backfill:
      enabled: true
      maxAge: "720h"      # 30 days
      maxEvents: 1000000
      batchSize: 500
      rateLimit: 1000     # events/second
    
    # Concurrent publish workers
    workers: 10           # See scale-up guide below
    
    # Failure handling
    failureHandling:
      maxRetries: 17      # ~30 minutes
      initialBackoff: "1s"
      maxBackoff: "5m"
      backoffMultiplier: 2.0
```

### Configuration Options

- **Metadata**: Custom key-value pairs that the exporter includes with every event (the cluster name is required)
- **Sink Endpoint**: HTTP/HTTPS URL to which the exporter posts CloudEvents. The exporter sends the OIDC access token with every request, also over HTTP. Thus, use an HTTPS URL.
- **OIDC Authentication**: OAuth2 client credentials for endpoint authentication
- **Backfill**: Optionally exports historical events when the exporter starts without a resume token, for example on the first startup
- **Workers**: Number of concurrent workers that publish events to the sink in parallel. The exporter advances resume tokens in strict order, independent of which worker finishes first. This keeps the at-least-once delivery guarantees. With concurrent publishing, events can arrive at the sink out of order. The default is `10`.
- **Retry Policy**: Exponential backoff configuration for failed publishes

## CloudEvents Format

The exporter transforms events into the CloudEvents v1.0 format:

```json
{
  "specversion": "1.0",
  "type": "com.nvidia.nvsentinel.health.v1",
  "source": "nvsentinel://production-us-west/healthevents",
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "time": "2025-11-27T10:30:00Z",
  "data": {
    "metadata": {
      "cluster": "production-us-west",
      "environment": "production",
      "region": "us-west-2"
    },
    "healthEvent": {
      "version": "v1",
      "agent": "gpu-health-monitor",
      "componentClass": "GPU",
      "checkName": "XIDError",
      "nodeName": "gpu-node-01",
      "message": "GPU XID error detected",
      "isFatal": true,
      "isHealthy": false,
      "recommendedAction": 2,
      "errorCode": ["XID_79"],
      "entitiesImpacted": [
        {
          "entityType": "GPU",
          "entityValue": "GPU-abc123"
        }
      ],
      "generatedTimestamp": "2025-11-27T10:30:00Z"
    }
  }
}
```

## Key Features

### CloudEvents Standard

Uses the industry-standard CloudEvents v1.0 format for broad compatibility with event processing platforms.

### Historical Backfill

When the exporter starts without a resume token, for example on the first deployment, it can export up to N days of historical events.

### Resume Token Tracking

Persists progress in the datastore for at-least-once delivery. On MongoDB, a resume token that becomes too old for the oplog can cause skipped events. For the details, see [Resume Token Storage](configuration/event-exporter.md#resume-token-storage).

### OIDC Authentication

Supports the OAuth2 client credentials flow with automatic token refresh for secure authentication.

### Exponential Backoff

Retries failed publishes with configurable exponential backoff (up to ~30 minutes by default).

### Custom Metadata

Adds custom metadata (for example, cluster, environment and region) to every event for filtering and routing.

### Rate Limiting

Configurable rate limiting for backfill, to prevent an overload of destination systems.

### Concurrent Workers

Publishes events in parallel with a configurable worker pool. The exporter advances resume tokens in strict order, independent of which worker finishes first. This keeps the at-least-once delivery guarantees. With concurrent publishing, events can arrive at the sink out of order. For sizing guidance, see the [configuration reference](configuration/event-exporter.md#scale-up-guide).

### Change Stream Based

Uses datastore change streams for real-time event delivery with minimal latency.
