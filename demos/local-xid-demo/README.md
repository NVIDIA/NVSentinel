# NVSentinel Local Demo: XID Error Detection and Node Quarantine

Welcome! This demo shows NVSentinel's core functionality running locally on your laptop. You'll see how NVSentinel automatically detects GPU failures and protects your cluster by cordoning faulty nodes.

> **💡 For this demo, no GPU Required!**  
> This demo runs on **any laptop** - no GPU needed! We simulate GPU failures by sending test events directly to NVSentinel, allowing you to see the full detection and response workflow without any special hardware.

## 🎯 What You'll Learn

1. **GPU Health Monitoring** - How NVSentinel detects hardware failures (XID errors)
2. **Automated Response** - How faulty nodes are automatically quarantined
3. **Event-Driven Architecture** - How health events flow through the system

## 📋 Prerequisites

**No GPU required!** This demo works on any laptop.

**System Requirements:**
- **Disk Space**: ~10GB free (for Docker images and KIND cluster)
- **Memory**: 4GB RAM minimum, 8GB recommended
- **CPU**: 2 cores minimum

**Required tools:**
- **Docker** - For running KIND (Kubernetes in Docker) ([install](https://docs.docker.com/get-docker/))
- **kubectl** - Kubernetes command-line tool ([install](https://kubernetes.io/docs/tasks/tools/))
- **kind** - Kubernetes IN Docker ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- **helm** - Kubernetes package manager ([install](https://helm.sh/docs/intro/install/))

**Optional (improves output formatting):**
- **curl** - For sending HTTP requests (usually pre-installed)
- **jq** - JSON processor for prettier output ([install](https://jqlang.github.io/jq/download/))

## 🚀 Quick Start

### Option 1: Automated Demo (Fast)

**Best for:** Quick overview, presentations, or if you're short on time.

**What you'll see:** The entire workflow runs automatically from cluster creation through error injection to verification. Great for getting a quick sense of NVSentinel's capabilities, but you won't see the details of each step.

```bash
# Run the complete demo (takes ~5-10 minutes)
make demo

# Clean up when done
make cleanup
```

### Option 2: Step-by-Step (Interactive - Recommended for Learning)

**Best for:** Understanding how NVSentinel works, learning the architecture, seeing logs and events in detail.

**What you'll learn:** By running each script individually, you'll see the cluster state before and after each action, understand the event flow, and have time to explore logs and Kubernetes resources at each stage.

**See below for expected output after each step!**

```bash
# Step 0: Create cluster and install NVSentinel
./scripts/00-setup.sh

# Step 1: View the healthy cluster
./scripts/01-show-cluster.sh

# Step 2: Inject a GPU fault (simulates hardware failure)
./scripts/02-inject-xid.sh

# Step 3: Verify node was cordoned
./scripts/03-verify-cordon.sh

# Clean up
./scripts/99-cleanup.sh
```


## 📚 Understanding the Demo

### What is an XID Error?

**XID errors** are NVIDIA GPU error codes that indicate hardware or driver failures. They're logged by the NVIDIA driver when a GPU experiences issues.

**Common XID Errors:**
- **XID 79**: "GPU has fallen off the bus" - critical hardware failure
- **XID 119**: RPC timeout from GPU - communication failure
- **XID 48**: Double Bit ECC error - memory corruption
- **XID 94**: Contained ECC error - correctable memory error

This demo simulates a **corrupt InfoROM fault**, which is a fatal GPU hardware error that requires the node to be removed from service.

### How NVSentinel Responds

When an XID error is detected, NVSentinel:

1. **Health Monitor** detects the XID error (from GPU monitor or syslog)
2. **Platform Connectors** receives the health event via gRPC
3. **MongoDB** stores the event in the persistent event database
4. **Fault Quarantine** watches for new events and evaluates rules
5. **Kubernetes API** - Node is cordoned (no new pods scheduled)
6. **Node Drainer** - (if enabled) Gracefully evicts running workloads
7. **Fault Remediation** - (if enabled) Triggers repair workflows

In this simplified demo, we focus on #1-5.

## 🏗️ Demo Architecture

This demo uses a **minimal NVSentinel deployment** with:

- **KIND Cluster** - 1 control plane + 1 worker node
- **Fake DCGM** - Simulates NVIDIA GPU monitoring (with NVML injection)
- **GPU Health Monitor** - Detects XID errors from DCGM
- **Platform Connectors** - gRPC server for receiving health events
- **Fault Quarantine** - Rule engine that cordons nodes on fatal errors
- **MongoDB** - Event storage and change streams

```text
┌───────────────────────────────────────────────────────┐
│  Your Laptop (KIND Cluster)                           │
│                                                       │
│  ┌──────────────────────────┐                         │
│  │ Worker Node              │                         │
│  │                          │                         │
│  │  * Fake DCGM (Injected)  │                         │
│  │  * GPU Health Monitor    │                         │
│  └──────────┬───────────────┘                         │
│             │ gRPC                                    │
│             ↓                                         │
│  ┌────────────────────────────────────────────────┐   │
│  │ NVSentinel Core                                │   │
│  │                                                │   │
│  │  ┌───────────────┐      ┌──────────────────┐   │   │
│  │  │  Platform     │─────>│    MongoDB       │   │   │
│  │  │  Connectors   │      │  (Event Store)   │   │   │
│  │  └───────────────┘      └────────┬─────────┘   │   │
│  │                                  │             │   │
│  │                         Change   │             │   │
│  │                         Stream   ↓             │   │
│  │                         ┌──────────────────┐   │   │
│  │                         │ Fault Quarantine │   │   │
│  │                         │  (CEL Rules)     │   │   │
│  │                         └────────┬─────────┘   │   │
│  │                                  │             │   │
│  │                                  ↓             │   │
│  │                         ┌──────────────────┐   │   │
│  │                         │  Kubernetes API  │   │   │
│  │                         │  (Cordon Node)   │   │   │
│  │                         └──────────────────┘   │   │
│  └────────────────────────────────────────────────┘   │
└───────────────────────────────────────────────────────┘
```


## 🔍 What Happens During the Demo

### Phase 0: Setup (00-setup.sh)

1. **Creates KIND cluster** with 1 worker node (minimal config)
2. **Installs cert-manager** (for TLS certificates)
3. **Installs NVSentinel** with minimal components:
   - Platform Connectors (event ingestion)
   - MongoDB (3-node replica set for change streams - adds ~2-3 min)
   - Simple Health Client (test tool)
   - Fault Quarantine (auto-cordon)
4. **Waits for all pods to be ready** (~5-6 minutes total)

### Phase 1: Initial State (01-show-cluster.sh)

Shows the healthy cluster:
- ✅ All nodes are `Ready` and `SchedulingEnabled`
- ✅ All NVSentinel pods are `Running`
- ✅ No health events in the database

**Expected output:**

```bash
$ kubectl get nodes
NAME                            STATUS   ROLES           AGE   VERSION
nvsentinel-demo-control-plane   Ready    control-plane   2m    v1.31.0
nvsentinel-demo-worker          Ready    <none>          2m    v1.31.0
```

Both nodes should be `Ready` with no scheduling restrictions.

### Phase 2: Failure Injection (02-inject-xid.sh)

**Injects a GPU hardware fault** into the fake DCGM service. The GPU Health Monitor detects this error automatically (just like it would with real GPU hardware) and NVSentinel quarantines the node.

**How it works:**
- We use `dcgmi test --inject` to simulate a corrupt InfoROM (a fatal GPU hardware fault)
- GPU Health Monitor polls DCGM every few seconds and detects the error
- NVSentinel automatically processes the event and cordons the node

This demonstrates the **actual production workflow** - no manual intervention, just like with real GPUs:

```json
{
  "version": 1,
  "agent": "gpu-health-monitor",
  "componentClass": "GPU",
  "checkName": "GpuXidError",
  "isFatal": true,
  "isHealthy": false,
  "message": "XID error 79: GPU has fallen off the bus",
  "recommendedAction": 15,
  "errorCode": ["79"],
  "entitiesImpacted": [{
    "entityType": "GPU",
    "entityValue": "0"
  }],
  "nodeName": "nvsentinel-demo-worker2"
}
```

The GPU Health Monitor detects this from DCGM and sends it via gRPC to Platform Connectors - exactly like production!

**Demo magic:** We use fake DCGM to simulate GPU faults without actual hardware - NVSentinel's detection and response is 100% authentic.

### Phase 3: Verification (03-verify-cordon.sh)

Confirms the automated response:
- 🔒 Worker node shows as `SchedulingDisabled` (cordoned)
- 📊 Node condition shows the health event
- 🎯 Fault Quarantine logs show the rule evaluation

**Expected output:**

```bash
$ kubectl get nodes
NAME                            STATUS                     ROLES           AGE   VERSION
nvsentinel-demo-control-plane   Ready                      control-plane   12m   v1.31.0
nvsentinel-demo-worker          Ready,SchedulingDisabled   <none>          12m   v1.31.0
```

Notice the worker node now shows `SchedulingDisabled` - NVSentinel automatically cordoned it after detecting the XID error! 🎉

### Cleanup (99-cleanup.sh)

Removes the KIND cluster and cleans up resources.

## 🎓 Learning Outcomes

After completing this demo, you'll understand:

1. **Event-Driven Architecture** - How health events flow through NVSentinel
2. **Kubernetes Integration** - How NVSentinel interacts with Kubernetes API
3. **Rule-Based Quarantine** - How CEL rules determine when to cordon nodes
4. **Production Readiness** - What a minimal NVSentinel deployment looks like

## ⚙️ Configuration

### Using a Different NVSentinel Version

By default, the demo uses NVSentinel v0.3.0 (the latest published release). To use a different version:

```bash
# Use a specific version (replace vX.Y.Z with your desired version)
NVSENTINEL_VERSION=vX.Y.Z ./scripts/00-setup.sh

# Or set it for the entire demo
NVSENTINEL_VERSION=vX.Y.Z make demo
```

### Using Development/Local Code

To test with local development code:
1. Build and push images to a registry accessible from KIND
2. Update the image tags in the Helm values file
3. See [DEVELOPMENT.md](../../DEVELOPMENT.md) for details

## 💾 Disk Space Management

The demo uses approximately **8-10GB** of disk space:
- KIND cluster images: ~2GB
- Container images (NVSentinel, MongoDB, DCGM): ~6-8GB

**To minimize disk usage:**
```bash
# After completing the demo, clean up immediately
make cleanup

# Or manually clean Docker
docker system prune -a -f --volumes
```

**If you're low on disk:**
- The demo creates a minimal deployment:
  - 1 MongoDB instance (single-member replica set for change streams)
  - 1 worker node (not 2+)
  - Persistence disabled (no storage volumes)
  - Only essential components enabled

## 🔧 Troubleshooting

### Out of disk space
```bash
# Check disk usage
df -h /

# Clean up Docker
docker system prune -a -f --volumes

# Delete old KIND clusters
kind get clusters
kind delete cluster --name <old-cluster-name>
```

### Cluster creation fails
```bash
# Clean up and retry
kind delete cluster --name nvsentinel-demo
./scripts/00-setup.sh
```

### Pods not starting
```bash
# Check pod status
kubectl get pods -n nvsentinel

# View logs
kubectl logs -n nvsentinel deployment/platform-connectors
kubectl logs -n nvsentinel deployment/simple-health-client
```

### Node not cordoning
```bash
# Check fault-quarantine logs
kubectl logs -n nvsentinel deployment/fault-quarantine

# Verify event was received
kubectl get events -A | grep XID
```

### Port conflicts
```bash
# Change the port used by simple-health-client
kubectl edit service -n nvsentinel simple-health-client
```

## 🚀 Next Steps

After trying this demo, explore more NVSentinel capabilities:

1. **Full Installation** - Deploy on a real cluster with GPU nodes ([Quick Start Guide](../../README.md#-quick-start))
2. **Production Configuration** - Enable node drainer and fault remediation ([Configuration Guide](../../distros/kubernetes/README.md))
3. **Custom Rules** - Write your own CEL rules for fault quarantine
4. **Scale Testing** - Try the [scale test suite](../../tests/README.md)
5. **Real GPU Monitoring** - Connect to actual NVIDIA GPUs with DCGM

## 📖 Additional Resources

- **[NVSentinel README](../../README.md)** - Project overview and features
- **[Architecture Guide](../../docs/OVERVIEW.md)** - Detailed system architecture
- **[Development Guide](../../DEVELOPMENT.md)** - Contributing and development setup
- **[Helm Chart Configuration](../../distros/kubernetes/README.md)** - All configuration options
- **[XID Error Reference](https://docs.nvidia.com/deploy/xid-errors/)** - NVIDIA's XID error documentation

## 🤝 Contributing

Found an issue with this demo? Want to improve it? We welcome contributions!

1. Check the [Contributing Guide](../../CONTRIBUTING.md)
2. Open an issue or pull request
3. Sign your commits with `git commit -s`

## 📄 License

This demo is part of NVSentinel and is licensed under the Apache License 2.0.

---

**Questions?** Start a [discussion](https://github.com/NVIDIA/NVSentinel/discussions) or [open an issue](https://github.com/NVIDIA/NVSentinel/issues).

**Enjoy the demo!** 🎉

