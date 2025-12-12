# Local Slinky Drain Demo

**Demonstration of NVSentinel's custom drain extensibility using the Slinky Drainer example plugin.**

This demo showcases the end-to-end custom drain workflow where node-drainer delegates pod eviction to an external controller (slinky-drainer) which coordinates with a cluster scheduler (mock-slurm-operator).

## 🎯 What This Demo Shows

**Custom Drain Flow:**
1. Health event injected → Platform Connectors → MongoDB
2. Node-drainer detects issue → Creates DrainRequest CR (custom drain)
3. Slinky-drainer watches DrainRequest → Annotates node
4. Mock-slurm-operator watches node annotation → Updates pod conditions
5. Slinky-drainer waits for conditions → Deletes pods → Marks CR complete
6. Node-drainer sees completion → Marks drain successful

**Key Concepts:**
- **Extensible drain architecture** - Node-drainer delegates to external controllers
- **Custom Resource-based coordination** - DrainRequest CR for workflow orchestration
- **Scheduler integration simulation** - Mock Slurm operator mimics real HPC scheduler behavior
- **Condition-based synchronization** - Wait for scheduler signals before pod deletion

## 📋 Prerequisites

- **Docker** - For building and running containers
- **kubectl** - Kubernetes CLI
- **kind** - Kubernetes in Docker (local clusters)
- **helm** - Kubernetes package manager
- **ko** - Go container image builder
- **go** - Go 1.25+ (for building components)

## 🚀 Quick Start

### 1. Setup Environment

```bash
make setup
```

This creates a KIND cluster with:
- ✅ cert-manager
- ✅ MongoDB
- ✅ Platform Connectors
- ✅ Fault Quarantine
- ✅ Node Drainer (with custom drain enabled)
- ✅ Slinky Drainer Plugin
- ✅ Mock Slurm Operator  
- ✅ Test Workloads (2 nginx pods in slinky namespace)

### 2. View Cluster Status

```bash
make show-cluster
```

Shows the deployed components, nodes, and workload pods.

### 3. Trigger Custom Drain

```bash
make inject-health-event
```

Injects a health event that triggers the automatic custom drain workflow:
- Health event → MongoDB
- Node-drainer creates DrainRequest CR
- Slinky-drainer processes the drain
- Mock-slurm-operator updates pod conditions
- Pods deleted, drain marked complete

### 4. Verify Drain Workflow

```bash
make verify-drain
```

Checks:
- DrainRequest CR created and completed
- Node annotation set by slinky-drainer
- Pod conditions updated by mock-slurm-operator
- Pods successfully deleted
- Drain marked successful in health event status

### 5. Cleanup

```bash
make cleanup
```

Deletes the KIND cluster and cleans up local Docker images.

## 🗂️ Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                         Demo Cluster                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Health Event Injection                                         │
│         │                                                       │
│         v                                                       │
│  Platform Connectors ──► MongoDB ◄── Node Drainer             │
│                                        │                        │
│                                        │ (creates)              │
│                                        v                        │
│                                DrainRequest CR                  │
│                                        │                        │
│                                        │ (watches)              │
│                                        v                        │
│                              Slinky Drainer Plugin              │
│                                        │                        │
│                         ┌──────────────┼──────────────┐        │
│                         │              │              │         │
│                         v              v              v         │
│                   Annotates Node   Waits for     Deletes Pods  │
│                         │         Conditions          │         │
│                         v              ^              v         │
│              Mock Slurm Operator       │        Pod Deletion   │
│              (watches annotation)      │                        │
│                         │              │                        │
│                         └─────► Updates Pod Conditions          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

## 📁 Components

### Core NVSentinel Components

1. **Platform Connectors** - Receives health events, stores in MongoDB
2. **Node Drainer** - Monitors health events, creates DrainRequest CRs
3. **MongoDB** - Stores health events and change stream tokens

### Custom Drain Plugins

1. **Slinky Drainer** (`plugins/slinky-drainer/`)
   - Watches DrainRequest CRs
   - Annotates nodes with cordon reason
   - Waits for scheduler signals (pod conditions)
   - Deletes pods after confirmation
   - Updates CR status

2. **Mock Slurm Operator** (`plugins/mock-slurm-operator/`)
   - Watches node annotations
   - Simulates Slurm scheduler behavior
   - Sets `SlurmNodeStateDrain=True` condition on pods
   - Signals readiness for pod deletion

## 🔧 Configuration

### Custom Drain Template

The node-drainer uses a Go template to generate DrainRequest CRs:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: DrainRequest
spec:
  nodeName: {{ .HealthEvent.NodeName }}
  checkName: {{ .HealthEvent.CheckName }}
  recommendedAction: {{ .HealthEvent.RecommendedAction.String }}
  errorCode:
  {{- range .HealthEvent.ErrorCode }}
  - {{ . }}
  {{- end }}
  healthEventID: {{ .EventID }}
  entitiesImpacted:
  {{- range .HealthEvent.EntitiesImpacted }}
  - type: {{ .EntityType }}
    value: {{ .EntityValue }}
  {{- end }}
  reason: "{{ .HealthEvent.Message }}"
```

### Node Drainer Config

```toml
[customDrain]
  enabled = true
  templateMountPath = "/etc/drain-template"
  templateFileName = "drain-template.yaml"
  namespace = "nvsentinel"
  apiGroup = "nvsentinel.nvidia.com"
  version = "v1alpha1"
  kind = "DrainRequest"
  statusConditionType = "Complete"
  statusConditionStatus = "True"
  timeout = "1800"  # 30 minutes
```

## 📊 Observability

### Watch DrainRequest CRs

```bash
kubectl get drainrequests -n nvsentinel -w
```

### Monitor Slinky Drainer Logs

```bash
kubectl logs -f deployment/slinky-drainer -n nvsentinel
```

### Monitor Mock Slurm Operator Logs

```bash
kubectl logs -f deployment/mock-slurm-operator -n nvsentinel
```

### Monitor Node Drainer Logs

```bash
kubectl logs -f deployment/node-drainer -n nvsentinel
```

### Check Node Annotations

```bash
kubectl get node nvsentinel-demo-worker -o jsonpath='{.metadata.annotations}'
```

### Check Pod Conditions

```bash
kubectl get pods -n slinky -o json | jq '.items[].status.conditions[] | select(.type=="SlurmNodeStateDrain")'
```

## 🧪 Manual Testing

### Create DrainRequest Manually

```bash
cat <<EOF | kubectl apply -f -
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: DrainRequest
metadata:
  name: manual-drain-test
  namespace: nvsentinel
spec:
  nodeName: nvsentinel-demo-worker
  checkName: ManualTest
  recommendedAction: drain
  errorCode: ["TEST-001"]
  healthEventID: "manual-test-event"
  reason: "Manual drain test"
EOF
```

### Watch the Workflow

```bash
# Terminal 1: Watch DR
kubectl get drainrequest manual-drain-test -n nvsentinel -w

# Terminal 2: Watch pods
kubectl get pods -n slinky -w

# Terminal 3: Watch node
kubectl get node nvsentinel-demo-worker -w
```

## 🐛 Troubleshooting

### DrainRequest Not Being Created

```bash
# Check node-drainer logs
kubectl logs deployment/node-drainer -n nvsentinel --tail=50

# Check MongoDB connection
kubectl exec -it mongodb-0 -n nvsentinel -- mongosh nvsentinel --eval "db.healthevents.find().pretty()"
```

### Slinky Drainer Not Processing

```bash
# Check slinky-drainer logs
kubectl logs deployment/slinky-drainer -n nvsentinel --tail=50

# Check RBAC permissions
kubectl auth can-i get drainrequests --as=system:serviceaccount:nvsentinel:slinky-drainer -n nvsentinel
```

### Mock Slurm Operator Not Updating Conditions

```bash
# Check mock-slurm-operator logs
kubectl logs deployment/mock-slurm-operator -n nvsentinel --tail=50

# Verify node annotation exists
kubectl get node nvsentinel-demo-worker -o yaml | grep -A5 annotations
```

### Pods Not Being Deleted

```bash
# Check pod conditions
kubectl get pods -n slinky -o json | jq '.items[].status.conditions'

# Check slinky-drainer reconciliation
kubectl logs deployment/slinky-drainer -n nvsentinel | grep "checkPodsReadyForDrain"
```

## 📚 Learn More

- [ADR-015: Custom Drain Extensibility](../../docs/designs/adr-015-custom-drain-extensibility.md)
- [Node Drainer Documentation](../../node-drainer/README.md)
- [Slinky Drainer Plugin](../../plugins/slinky-drainer/README.md)
- [Mock Slurm Operator](../../plugins/mock-slurm-operator/README.md)

## 🤝 Contributing

This demo is an example implementation. For production use:
- Replace mock-slurm-operator with your actual scheduler integration
- Adjust timeouts and intervals for your environment
- Implement proper monitoring and alerting
- Add authentication/authorization as needed
- Use persistent MongoDB with backup/restore

## 📝 License

Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
Licensed under the Apache License, Version 2.0.
