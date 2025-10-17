# NVSentinel Release Testing

Test published NVSentinel Helm charts using Tilt with simulated GPU environments.

## Quick Start

```bash
# Set required version
export NVSENTINEL_VERSION=v0.0.3

# Start environment (from root directory)
tilt up -f tilt/release/Tiltfile.release

# Access Prometheus UI
open http://localhost:9090
```

## What This Deploys

- **NVSentinel Stack**: Complete health monitoring system from published chart
- **50 Fake GPU Nodes**: KWOK-simulated nodes with NVIDIA drivers/DCGM
- **Infrastructure**: cert-manager, Prometheus, MongoDB
- **Test Client**: Generates health events for testing

## Environment Variables

| Variable | Required | Default | Description                    |
|----------|----------|---------|--------------------------------|
| `NVSENTINEL_VERSION` | ✅ | _none_ | Chart version (e.g., `v0.5.0`) |
| `NVSENTINEL_VALUES` | ❌ | `./values-release.yaml` | Custom values file             |
| `NUM_GPU_NODES` | ❌ | `50` | Number of fake GPU nodes       |

## Files

- **`Tiltfile.release`**: Main deployment configuration with comprehensive comments
- **`values-release.yaml`**: NVSentinel chart values optimized for testing
- **`README.md`**: This guide

## Advanced Usage

### Secret for private images

```bash
kubectl create secret docker-registry nvidia-ngcuser-pull-secret \
  --docker-server=ghcr.io \
  --docker-username=YOUR_USER_ID \
  --docker-password=YOUR_PASSWORD \
  --namespace=nvsentinel
```

### Custom Chart Version
```bash
NVSENTINEL_VERSION=v0.0.4 tilt up -f tilt/release/Tiltfile.release
```

### Custom Configuration
```bash
# Create custom values
cp values-release.yaml my-values.yaml
# Edit my-values.yaml...

# Use custom values
NVSENTINEL_VALUES=./my-values.yaml tilt up -f tilt/release/Tiltfile.release
```

### Scale Fake Nodes
```bash
NUM_GPU_NODES=100 tilt up -f tilt/release/Tiltfile.release
```

## Validation

Verify deployment health:
```bash
# Run comprehensive validation
./scripts/validate-nvsentinel.sh --version v0.0.3 --verbose

# Quick health check
kubectl get pods -n nvsentinel
```

## Cleanup

```bash
tilt down -f tilt/release/Tiltfile.release
```

---

For development builds from source, use the main `Tiltfile` instead.
