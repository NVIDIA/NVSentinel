# Node Metadata Package

The `nodemetadata` package augments health events with node metadata from Kubernetes, including provider IDs, labels, and topology information. It provides a thread-safe caching layer to minimize Kubernetes API calls and optimize performance.

## Features

- **Provider ID Decoding**: Automatically extracts cloud provider information (AWS, GCP, Azure, OCI) from Kubernetes node provider IDs
- **Label Filtering**: Includes only allowed node labels in health events
- **LRU Cache with TTL**: Uses battle-tested `hashicorp/golang-lru` for storage
- **Fetch Deduplication**: Double-check pattern with mutex prevents duplicate concurrent API calls
- **Simple & Reliable**: Clean architecture using standard library sync primitives
- **Best-Effort Augmentation**: Failures don't block health event processing

## Architecture

```
Health Event → Processor.AugmentHealthEvent()
                    ↓
          Cache.GetOrFetch(nodeName)
         (hashicorp/golang-lru + mutex)
              ↓           ↓
          Cache Hit    Cache Miss
              ↓           ↓
         Return     Double-Check Pattern:
                    1. Check cache (fast path)
                    2. Acquire mutex lock
                    3. Check cache again
                    4. Fetch from K8s API
                    5. Cache result
                          ↓
                    K8s API Get Node
                          ↓
                    Decode Provider ID
                          ↓
                    Filter Labels
                          ↓
                    Return (cached for future)
```

**Component Responsibilities:**
- **Processor**: Orchestrates augmentation workflow and business logic
- **Cache**: Storage (hashicorp/golang-lru) + coordination (simple mutex with double-check pattern)
- **Provider**: Decodes cloud provider IDs (AWS, GCP, Azure, OCI)

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
    "node.kubernetes.io/instance-type",
    "nvidia.com/gpu.present",
    "rack",
    "capacity-tranche"
  ]
}
```

### Configuration Options

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `nodeMetadataAugmentationEnabled` | string | `"false"` | Enable/disable augmentation |
| `nodeMetadataCacheSize` | int | `50` | Maximum cache entries |
| `nodeMetadataCacheTTLSeconds` | int | `3600` | Cache entry TTL in seconds |
| `nodeMetadataAPITimeoutSeconds` | int | `2` | Kubernetes API call timeout |
| `nodeMetadataQPS` | float | `5.0` | K8s client rate limit (QPS) |
| `nodeMetadataBurst` | int | `10` | K8s client burst limit |
| `nodeMetadataMaxRetries` | int | `3` | Max API call retries |
| `nodeMetadataDecodeProviderID` | string | `"true"` | Decode provider ID into fields |
| `nodeMetadataAllowedLabels` | []string | Comprehensive defaults | Node labels to include (see [Default Labels](#default-labels) below) |

### Default Labels

If `nodeMetadataAllowedLabels` is not specified in the config, the following labels are included by default:

**Topology Labels** (failure domain analysis):
- `topology.kubernetes.io/zone` - Availability zone
- `topology.kubernetes.io/region` - Cloud region
- `failure-domain.beta.kubernetes.io/zone` - Legacy zone label
- `failure-domain.beta.kubernetes.io/region` - Legacy region label

**Instance Type** (cost attribution):
- `node.kubernetes.io/instance-type` - Instance/VM type (e.g., p5.48xlarge)
- `beta.kubernetes.io/instance-type` - Legacy instance type label

**GPU-Specific Labels**:
- `nvidia.com/gpu.present` - GPU node identification
- `nvidia.com/gpu.deploy.dcgm` - DCGM deployment topology
- `nvidia.com/gpu.deploy.driver` - Driver deployment topology  
- `nvidia.com/gpu.count` - Number of GPUs
- `nvsentinel.dgxc.nvidia.com/dcgm.version` - DCGM version (3.x or 4.x)
- `nvsentinel.dgxc.nvidia.com/driver.installed` - Driver installation status

**Physical Topology** (if available):
- `rack` - Physical rack identifier
- `datacenter` - Datacenter identifier
- `row` - Physical row in datacenter
- `capacity-tranche` - Capacity reservation group

**Workload Classification**:
- `workload-type` - Workload classification (e.g., "gpu", "training")

**Note:** High-churn labels like `dgxc.nvidia.com/nvsentinel-state` are intentionally excluded to prevent cache thrashing.

## Kubernetes Deployment Configuration

### Enable Node Metadata Augmentation

To enable node metadata augmentation in your NVSentinel deployment, configure the platform-connectors via Helm values:

#### Minimal Configuration (Enable with Defaults)

```yaml
# values.yaml or custom values file
platformConnectors:
  config:
    # Enable node metadata augmentation (uses all defaults)
    nodeMetadataAugmentationEnabled: "true"
```

This enables the feature with sensible defaults:
- **Cache Size:** 50 entries
- **Cache TTL:** 1 hour (3600 seconds)
- **API Timeout:** 2 seconds
- **Allowed Labels:** 18 recommended labels (topology, instance type, GPU labels, etc.)
- **Provider ID Decoding:** Enabled

#### Custom Configuration

```yaml
# values.yaml
platformConnectors:
  config:
    # Enable the feature
    nodeMetadataAugmentationEnabled: "true"
    
    # Cache Configuration
    nodeMetadataCacheSize: 100                # Increase if you have more unique nodes
    nodeMetadataCacheTTLSeconds: 7200        # 2 hours (node metadata rarely changes)
    
    # API Configuration
    nodeMetadataAPITimeoutSeconds: 5         # Increase if K8s API is slow
    nodeMetadataQPS: 10.0                    # Rate limit for K8s API calls
    nodeMetadataBurst: 20                    # Burst allowance
    
    # Provider ID Decoding
    nodeMetadataDecodeProviderID: "true"     # Decode aws:///us-west-2a/i-123 into fields
    
    # Custom Allowed Labels (overrides defaults)
    nodeMetadataAllowedLabels:
      - "topology.kubernetes.io/zone"
      - "topology.kubernetes.io/region"
      - "node.kubernetes.io/instance-type"
      - "nvidia.com/gpu.present"
      - "nvidia.com/gpu.count"
      - "rack"                                # Custom label for physical rack
      - "capacity-tranche"                    # Custom label for capacity groups
      - "workload-type"                       # Custom label for workload classification
```

### Configuration Recommendations

#### For Small Clusters (<50 nodes)
```yaml
nodeMetadataAugmentationEnabled: "true"
nodeMetadataCacheSize: 50              # Default is sufficient
nodeMetadataCacheTTLSeconds: 3600     # 1 hour
```

#### For Large Clusters (50-200 nodes)
```yaml
nodeMetadataAugmentationEnabled: "true"
nodeMetadataCacheSize: 200             # Increase cache size
nodeMetadataCacheTTLSeconds: 7200     # 2 hours (reduce API calls)
nodeMetadataQPS: 10.0                 # Higher QPS for more nodes
```

#### For Multi-Rack Deployments
```yaml
nodeMetadataAugmentationEnabled: "true"
nodeMetadataAllowedLabels:
  - "topology.kubernetes.io/zone"
  - "topology.kubernetes.io/region"
  - "node.kubernetes.io/instance-type"
  - "nvidia.com/gpu.present"
  - "rack"                             # Physical rack ID
  - "datacenter"                       # Datacenter ID
  - "row"                              # Physical row
  - "capacity-tranche"                 # Capacity reservation group
```

### Verifying Configuration

After deploying with node metadata augmentation enabled:

1. **Check if enabled:**
```bash
kubectl logs -n nvsentinel daemonset/nvsentinel-platform-connectors | grep "Node metadata"
# Expected output:
# Node metadata processor initialized cacheSize=50 cacheTTL=1h0m0s ...
```

2. **Verify health events contain metadata:**
```bash
# Query MongoDB for a recent health event
kubectl exec -n nvsentinel mongodb-0 -- mongosh --eval '
  db.healthEvents.findOne(
    {},
    { "metadata": 1, "nodeName": 1 }
  )
'

# Expected output should include:
# {
#   "nodeName": "gpu-node-123",
#   "metadata": {
#     "node.provider": "aws",
#     "node.zone": "us-west-2a",
#     "node.region": "us-west-2",
#     "node.instanceID": "i-0123456789abcdef",
#     "node.label.topology.kubernetes.io/zone": "us-west-2a",
#     "node.label.node.kubernetes.io/instance-type": "p5.48xlarge",
#     ...
#   }
# }
```

3. **Monitor cache performance (check logs):**
```bash
kubectl logs -n nvsentinel daemonset/nvsentinel-platform-connectors | grep -i cache
```

### Disabling Node Metadata Augmentation

To disable the feature (e.g., for testing):

```yaml
platformConnectors:
  config:
    nodeMetadataAugmentationEnabled: "false"
```

Health events will continue to be processed, but without additional node metadata.

### Troubleshooting

#### Issue: Health events don't have metadata

**Check 1:** Verify feature is enabled
```bash
kubectl logs -n nvsentinel daemonset/nvsentinel-platform-connectors | grep "Node metadata augmentation is disabled"
```

**Check 2:** Verify no errors
```bash
kubectl logs -n nvsentinel daemonset/nvsentinel-platform-connectors | grep -i "failed to get metadata"
```

**Check 3:** Verify RBAC permissions
```bash
kubectl auth can-i get nodes --as=system:serviceaccount:nvsentinel:nvsentinel-platform-connectors
# Should return "yes"
```

#### Issue: High Kubernetes API load

**Solution:** Increase cache TTL and size
```yaml
nodeMetadataCacheTTLSeconds: 7200   # 2 hours instead of 1 hour
nodeMetadataCacheSize: 200          # Larger cache
```

#### Issue: Missing custom labels

**Check:** Ensure labels exist on nodes
```bash
kubectl get nodes --show-labels | grep -E "rack|capacity-tranche"
```

**Solution:** Add labels to allowlist
```yaml
nodeMetadataAllowedLabels:
  - "your-custom-label"
```

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

## Performance Characteristics

- **Cache Hit**: ~10µs (no API call, fast `hashicorp/golang-lru` lookup)
- **Cache Miss**: ~2-50ms (depends on K8s API latency)
- **Concurrent Fetch Deduplication**: Double-check pattern minimizes duplicate fetches
- **Memory**: ~1KB per cached node (configurable via cache size)
- **TTL Eviction**: Automatic, handled by library without background goroutines
- **Concurrency**: Simple mutex-based coordination (no complex state management)

## Testing

The package includes comprehensive unit tests:

### Test Suites

**1. Cache Tests** (`cache_test.go`) - 12 tests
- Storage layer (LRU, TTL, eviction)
- Concurrent access and thread-safety
- GetOrFetch with double-check pattern
- Fetch deduplication
- Context cancellation

**2. Processor Tests** (`processor_test.go`) - 13 tests
- Health event augmentation
- Provider ID decoding
- Label filtering
- Cache integration
- Error handling

**3. Provider Tests** (`provider_test.go`) - 10 tests
- AWS, GCP, Azure, OCI provider ID decoding
- Edge cases and error handling

**4. Config Tests** (`config_test.go`) - 2 tests
- Configuration validation
- Default values

### Running Tests

Run all tests:
```bash
go test ./pkg/nodemetadata/...
```

Run with coverage:
```bash
go test -cover ./pkg/nodemetadata/...
```

Run with race detector:
```bash
go test -race ./pkg/nodemetadata/...
```

**Test Coverage:** 91.7% of statements

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

