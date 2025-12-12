# NVSentinel Event Generator

A unified, parameterizable event generator for NVSentinel scale testing.

## Version

**v1** - Compatible with NVSentinel v0.3.0+

**One image, all test scenarios!** Everything is configured via ConfigMaps and command-line arguments.

## Features

This single event generator supports multiple operating modes:

### 1. Continuous Generation Mode (Scale Testing)

Generates events continuously at a configurable rate via `EVENT_RATE` environment variable. Used for API server and MongoDB load testing.

**Event Distribution:**
- 64% (80/125) - Healthy GPU events
- 24% (30/125) - System info events  
- 8%  (10/125) - 🔴 Fatal GPU errors (XID 79) - **Triggers node cordoning**
- 4%  (5/125)  - NVSwitch warnings

**Usage:**
```yaml
env:
- name: EVENT_RATE
  value: "0.02"  # Events per second (0.02 = 1.2 events/min per node)
```

### 2. Background + On-Demand Mode (Latency Testing)

Generates background healthy events + responds to SIGUSR1 signals for fatal event injection. Used for FQM latency testing.

**Usage:**
```yaml
args:
- "-socket=/var/run/nvsentinel/nvsentinel.sock"
- "-background=true"
- "-interval=15s"
```

### 3. On-Demand Only Mode (Sequential Testing)

Only generates events when receiving SIGUSR1 signals. Used for sequential latency measurements.

**Usage:**
```yaml
args:
- "-socket=/var/run/nvsentinel/nvsentinel.sock"
```

## Configuration

### Environment Variables

| Variable | Description | Example | Required |
|----------|-------------|---------|----------|
| `NODE_NAME` | Kubernetes node name | `ip-10-0-1-5` | Yes |
| `EVENT_RATE` | Events per second (enables continuous mode) | `0.25`, `0.75`, `2.0` | No |

### Command-Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `-socket` | Unix socket path | `/var/run/nvsentinel/nvsentinel.sock` |
| `-background` | Enable background generation | `false` |
| `-interval` | Background event interval | `15s` |

## Building

```bash
# Set your registry (change this to your own registry)
export IMAGE_REGISTRY="your-registry.com/your-project"

# Build Docker image
docker build -t ${IMAGE_REGISTRY}/event-generator:v1 .

# Push to your registry
docker push ${IMAGE_REGISTRY}/event-generator:v1

# Update manifests to use your registry
cd ../manifests
sed -i "s|ghcr.io/nvidia/nvsentinel|${IMAGE_REGISTRY}|g" event-generator-daemonset.yaml
```


## Testing Locally

```bash
# Build locally
go build -o event-generator .

# Test continuous mode
export NODE_NAME="test-node"
export EVENT_RATE="1.0"
./event-generator -socket=/tmp/test.sock

# Test background mode
./event-generator -socket=/tmp/test.sock -background=true -interval=5s

# Send SIGUSR1 to trigger fatal event
kill -USR1 <pid>
```

## Deployment Examples

### ONE IMAGE - Multiple Configurations!

**All test scenarios use the same image:** `your-registry/event-generator:v1`

**Change test loads by switching ConfigMaps:**

```bash
# Light Load (30 events/sec @ 1500 nodes = 3-4× production peak)
kubectl apply -f ../manifests/event-generator-config-light.yaml
kubectl apply -f ../manifests/event-generator-daemonset.yaml

# Medium Load (100 events/sec @ 1500 nodes = 10-12× production peak)
kubectl apply -f ../manifests/event-generator-config-medium.yaml
kubectl apply -f ../manifests/event-generator-daemonset.yaml

# Heavy Load (300 events/sec @ 1500 nodes = 30-35× production peak)
kubectl apply -f ../manifests/event-generator-config-heavy.yaml
kubectl apply -f ../manifests/event-generator-daemonset.yaml
```

### ConfigMap Examples

These examples assume a **1500-node cluster**. Adjust `EVENT_RATE` based on your cluster size to achieve desired total event rate.

**Light Load (Conservative baseline):**
```yaml
data:
  EVENT_RATE: "0.02"  # 1.2 events/min/node
  # At 1500 nodes: 30 events/sec cluster-wide (3-4× production peak)
```

**Medium Load (Moderate stress):**
```yaml
data:
  EVENT_RATE: "0.067"  # 4.0 events/min/node
  # At 1500 nodes: 100 events/sec cluster-wide (10-12× production peak)
```

**Heavy Load (High stress):**
```yaml
data:
  EVENT_RATE: "0.2"  # 12.0 events/min/node
  # At 1500 nodes: 300 events/sec cluster-wide (30-35× production peak)
```

**For different cluster sizes:**
```
Total events/sec = EVENT_RATE × number_of_nodes
EVENT_RATE = desired_total_events_per_sec / number_of_nodes
```

## Verified Pipeline

```
Event Generator v1
    ↓ gRPC Unix Socket (/var/run/nvsentinel/nvsentinel.sock)
Platform Connector v0.4.0
    ↓ MongoDB Write
Fault Quarantine Module v0.4.0
    ↓ Fatal Event Detection (8% of events)
✅ Node Cordoned!
```

## gRPC Compatibility

Uses NVSentinel data models v0.3.0+:
- Package: `github.com/nvidia/nvsentinel/data-models/pkg/protos`
- Service: `datamodels.PlatformConnector`
- RPC: `HealthEventOccurredV1`
- Enums: `RecommendedAction_NONE`, `RecommendedAction_COMPONENT_RESET`

## Statistics Logging

In continuous mode, the generator logs statistics every 100 events:

```
📊 Stats: 1000 events sent (0.25 events/sec), 100.0% success rate
```

Individual event successes are logged occasionally (1% of events) to reduce log noise.

## Troubleshooting

### "unknown service" gRPC error

**Cause:** Version mismatch between event generator and platform connectors

**Solution:** Ensure platform connectors are running v0.3.0+

### Events not being generated

**Check:**
1. `NODE_NAME` environment variable is set
2. Unix socket exists: `ls -la /var/run/nvsentinel/nvsentinel.sock`
3. Platform connector pod is running on same node
4. Check logs: `kubectl logs -n nvsentinel <pod-name>`

### Low event rate

**Check:**
1. `EVENT_RATE` is set correctly
2. No CPU throttling: `kubectl top pods -n nvsentinel`
3. gRPC latency is reasonable (should be <1ms)

---

**Last Updated:** November 24, 2025  
**Compatible with:** NVSentinel v0.3.0+
**Image Version:** v1

