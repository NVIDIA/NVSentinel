# Node Metadata Package

The `nodemetadata` package augments health events with node metadata from Kubernetes, including provider IDs, labels, and topology information. It provides a thread-safe caching layer to minimize Kubernetes API calls and optimize performance.

## Features

- **Provider ID Decoding**: Automatically extracts cloud provider information (AWS, GCP, Azure, OCI) from Kubernetes node provider IDs
- **Label Filtering**: Includes only allowed node labels in health events
- **LRU Cache**: Thread-safe caching with configurable size and TTL
- **Singleflight Pattern**: Deduplicates concurrent requests for the same node
- **Best-Effort Augmentation**: Failures don't block health event processing
- **Prometheus Metrics**: Comprehensive metrics for monitoring cache performance and API calls

## Architecture

```
Health Event → Processor.AugmentHealthEvent()
                    ↓
          Cache.Get(nodeName)
              ↓           ↓
          Cache Hit    Cache Miss
              ↓           ↓
         Return       Singleflight
                          ↓
                    K8s API Get Node
                          ↓
                    Decode Provider ID
                          ↓
                    Filter Labels
                          ↓
                    Cache & Return
```

## Configuration

Configuration is loaded from the platform connector config map:

```json
{
  "nodeMetadataAugmentationEnabled": "true",
  "nodeMetadataCacheSize": 1000,
  "nodeMetadataCacheTTLSeconds": 3600,
  "nodeMetadataAPITimeoutSeconds": 2,
  "nodeMetadataQPS": 5.0,
  "nodeMetadataBurst": 10,
  "nodeMetadataMaxRetries": 3,
  "nodeMetadataDecodeProviderID": "true",
  "nodeMetadataAllowedLabels": [
    "topology.kubernetes.io/zone",
    "topology.kubernetes.io/region",
    "node.kubernetes.io/instance-type"
  ]
}
```

### Configuration Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `nodeMetadataAugmentationEnabled` | string | `"false"` | Enable/disable augmentation |
| `nodeMetadataCacheSize` | int | `1000` | Maximum cache entries |
| `nodeMetadataCacheTTLSeconds` | int | `3600` | Cache entry TTL in seconds |
| `nodeMetadataAPITimeoutSeconds` | int | `2` | Kubernetes API call timeout |
| `nodeMetadataQPS` | float | `5.0` | K8s client rate limit (QPS) |
| `nodeMetadataBurst` | int | `10` | K8s client burst limit |
| `nodeMetadataMaxRetries` | int | `3` | Max API call retries |
| `nodeMetadataDecodeProviderID` | string | `"true"` | Decode provider ID into fields |
| `nodeMetadataAllowedLabels` | []string | `[]` | Node labels to include |

## Usage

### Initialization

```go
import (
    "context"
    "github.com/nvidia/nvsentinel/platform-connectors/pkg/nodemetadata"
)

// Load config from map
cfg, err := nodemetadata.NewConfigFromMap(configMap)
if err != nil {
    return err
}

// Create processor
processor, err := nodemetadata.NewProcessor(ctx, cfg)
if err != nil {
    return err
}

// Start background tasks (cache cleanup)
go processor.Start(ctx)

// Augment health events
err = processor.AugmentHealthEvent(ctx, healthEvent)
if err != nil {
    // Handle error (best-effort)
}

// Cleanup
processor.Stop()
```

### Health Event Augmentation

The processor adds metadata fields to `HealthEvent.Metadata`:

#### Provider ID Fields (when decoded)

**AWS**:
- `node.providerID`: Full provider ID
- `node.provider`: `"aws"`
- `node.zone`: Availability zone (e.g., `"us-west-2a"`)
- `node.region`: Region (e.g., `"us-west-2"`)
- `node.instanceID`: EC2 instance ID

**GCP**:
- `node.providerID`: Full provider ID
- `node.provider`: `"gcp"`
- `node.projectID`: GCP project ID
- `node.zone`: Zone (e.g., `"us-central1-a"`)
- `node.region`: Region (e.g., `"us-central1"`)
- `node.instanceName`: Instance name

**Azure**:
- `node.providerID`: Full provider ID
- `node.provider`: `"azure"`
- `node.subscriptionID`: Azure subscription ID
- `node.resourceGroup`: Resource group name
- `node.vmName`: VM or VMSS name

**OCI**:
- `node.providerID`: Full provider ID
- `node.provider`: `"oci"`
- `node.instanceID`: OCI instance OCID
- `node.region`: Region code

#### Label Fields

Labels are prefixed with `node.label.`:
- `node.label.topology.kubernetes.io/zone`: Zone label
- `node.label.topology.kubernetes.io/region`: Region label
- etc.

## Metrics

The package exposes Prometheus metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `nodemetadata_augmentation_total` | Counter | Total augmentation attempts |
| `nodemetadata_augmentation_success_total` | Counter | Successful augmentations |
| `nodemetadata_augmentation_failures_total` | Counter | Failed augmentations |
| `nodemetadata_augmentation_duration_milliseconds` | Histogram | Augmentation duration |
| `nodemetadata_cache_hits_total` | Counter | Cache hits |
| `nodemetadata_cache_misses_total` | Counter | Cache misses |
| `nodemetadata_cache_size` | Gauge | Current cache size |
| `nodemetadata_cache_evictions_total` | Counter | Cache evictions |
| `nodemetadata_k8s_api_calls_total` | Counter | Kubernetes API calls |
| `nodemetadata_k8s_api_calls_success_total` | Counter | Successful API calls |
| `nodemetadata_k8s_api_calls_failures_total` | Counter | Failed API calls |
| `nodemetadata_k8s_api_call_duration_milliseconds` | Histogram | API call duration |

## Performance Characteristics

- **Cache Hit**: ~10µs (no API call)
- **Cache Miss**: ~2-50ms (depends on K8s API latency)
- **Singleflight**: Only 1 API call per node regardless of concurrent requests
- **Memory**: ~1KB per cached node (configurable via cache size)

## Testing

Run tests:
```bash
cd platform-connectors
go test ./pkg/nodemetadata/...
```

Run with coverage:
```bash
go test -cover ./pkg/nodemetadata/...
```

## Example Health Event

Before augmentation:
```json
{
  "nodeName": "gpu-node-123",
  "message": "XID 79 detected",
  "metadata": {}
}
```

After augmentation:
```json
{
  "nodeName": "gpu-node-123",
  "message": "XID 79 detected",
  "metadata": {
    "node.providerID": "aws:///us-west-2a/i-0123456789abcdef",
    "node.provider": "aws",
    "node.zone": "us-west-2a",
    "node.region": "us-west-2",
    "node.instanceID": "i-0123456789abcdef",
    "node.label.topology.kubernetes.io/zone": "us-west-2a",
    "node.label.topology.kubernetes.io/region": "us-west-2"
  }
}
```

## Thread Safety

All components are thread-safe and can be called concurrently from multiple goroutines.

## Error Handling

The processor uses best-effort augmentation:
- If metadata fetch fails, the error is logged but the health event continues processing
- Transient errors (network, API unavailable) are logged at WARN level
- The processor never blocks health event delivery

