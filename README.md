# NVSentinel

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5.svg?logo=kubernetes&logoColor=white)](https://kubernetes.io/)
[![Helm](https://img.shields.io/badge/Helm-3.0+-0F1689.svg?logo=helm&logoColor=white)](https://helm.sh/)

**GPU Node Resilience System for Kubernetes**

NVSentinel is a comprehensive collection of Kubernetes services that automatically detect, classify, and remediate hardware and software faults in GPU nodes. Designed for GPU clusters, it ensures maximum uptime and seamless fault recovery in high-performance computing environments.

> [!WARNING]
> **Experimental Preview Release**
> This is an experimental/preview release of NVSentinel. Use at your own risk in production environments. The software is provided "as is" without warranties of any kind. Features, APIs, and configurations may change without notice in future releases. For production deployments, thoroughly test in non-critical environments first.

## 🚀 Quick Start

### Prerequisites

- Kubernetes cluster 1.25+
- Helm 3.0+
- NVIDIA GPU Operator (includes DCGM service required for GPU monitoring)

### Installation

```bash
# Install from GitHub Container Registry (recommended)
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel --version vX.Y.Z

# Or install with custom values
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel --version vX.Y.Z -f values.yaml

# To see available versions
helm search repo oci://ghcr.io/nvidia/nvsentinel --versions
```

> **Authentication Note**: The NVSentinel packages are hosted as private packages. You may need to authenticate with GitHub Container Registry:
> ```bash
> # Login to GitHub Container Registry
> echo $GITHUB_TOKEN | helm registry login ghcr.io -u $GITHUB_USERNAME --password-stdin
> ```


> **Note**: DCGM (Data Center GPU Manager) is included with the NVIDIA GPU Operator and provides the telemetry data required for GPU health monitoring.

## ✨ Key Features

- **🔍 Comprehensive Monitoring**: Real-time detection of GPU, NVSwitch, and system-level failures
- **🔧 Automated Remediation**: Intelligent fault handling with cordon, drain, and break-fix workflows
- **📦 Modular Architecture**: Pluggable health monitors with standardized gRPC interfaces
- **🔄 High Availability**: Kubernetes-native design with replica support and leader election
- **⚡ Real-time Processing**: Event-driven architecture with immediate fault response
- **📊 Persistent Storage**: MongoDB-based event store with change streams for real-time updates
- **🛡️ Graceful Handling**: Coordinated workload eviction with configurable timeouts

## 🧪 Quick test with any Kubernetes cluster

**Prerequisites**: Kubernetes 1.25+, Helm 3.0+, NVIDIA GPU Operator installed

NVSentinel requires a few infrastructure components for certificate management and monitoring. Here's a complete installation walkthrough:

### 1. Install cert-manager (TLS certificates)

NVSentinel uses mTLS for secure MongoDB connections. Install cert-manager to handle certificate lifecycle:

```bash
helm repo add jetstack https://charts.jetstack.io --force-update
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.19.1 --set installCRDs=true \
  --wait
```

### 2. Install Prometheus (metrics collection)

Install Prometheus operator for NVSentinel metrics and monitoring. We disable unnecessary components for a minimal setup:

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts --force-update
helm upgrade --install prometheus prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace \
  --set prometheus.enabled=true \
  --set alertmanager.enabled=false \
  --set grafana.enabled=false \
  --set kubeStateMetrics.enabled=false \
  --set nodeExporter.enabled=false \
  --wait
```

### 3. Install NVSentinel

Now install NVSentinel itself. Use the latest stable version from the releases page:

```bash
NVSENTINEL_VERSION=v0.1.0  # Check releases for latest version
BASE_DIR="$(pwd)"

helm upgrade --install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --create-namespace \
  --namespace nvsentinel \
  --version "$NVSENTINEL_VERSION" \
  --values "${BASE_DIR}/tilt/release/values-release.yaml" \
  --timeout 15m \
  --wait-for-jobs \
  --wait
```

> **Note**: The example uses [`tilt/release/values-release.yaml`](tilt/release/values-release.yaml) which is optimized for testing with fake GPU nodes. For production clusters, customize the values or omit the `--values` flag to use chart defaults.

### 4. Verify installation

Check that all pods are running:

```bash
kubectl get pods -n nvsentinel
kubectl get nodes  # Should show your GPU nodes

# Run comprehensive validation
./scripts/validate-nvsentinel.sh --version v0.1.0 --verbose
```

> **Tip**: Use the [`scripts/validate-nvsentinel.sh`](scripts/validate-nvsentinel.sh) script for comprehensive deployment validation including image versions, pod health, and certificate status.

**Next steps**: By default, only health monitoring is enabled. See [Configuration](#-configuration) to enable fault quarantine and remediation modules for production clusters.


## 🏗️ Architecture

NVSentinel follows a microservices architecture with modular health monitors and core processing modules:

```mermaid
graph TB
    subgraph "Health Monitors"
        GPU["GPU Health Monitor"]
        SYS["Syslog Health Monitor"]
        CSP["CSP Health Monitor"]
    end
    
    subgraph "Core Processing"
        PC["Platform Connectors"]
        STORE[("MongoDB Store<br/>Event Database")]
        FQ["Fault Quarantine"]
        ND["Node Drainer"]
        FR["Fault Remediation"]
        HEA["Health Events Analyzer"]
    end
    
    GPU --> PC
    SYS --> PC
    CSP --> PC

    
    PC --> STORE
    
    FQ -.->|watches| STORE
    ND -.->|watches| STORE
    FR -.->|watches| STORE
    HEA -.->|watches| STORE
```

**Data Flow**:
1. **Health Monitors** detect hardware/software faults and send events via gRPC
2. **Platform Connectors** receive and persist events to MongoDB
3. **Core Modules** independently watch MongoDB for relevant events via change streams
4. **Each module** acts autonomously based on their configured rules and policies

> **Note**: Modules operate independently and don't communicate directly with each other. All coordination happens through the shared MongoDB event store using change streams.

## ⚙️ Configuration

### Quick Configuration

Enable/disable modules and set global options:

```yaml
global:
  # Global settings
  dryRun: false  # Enable for testing without actual actions
  
  # Health Monitors (enabled by default)
  gpuHealthMonitor:
    enabled: true
  syslogHealthMonitor:
    enabled: true
  
  # Core Modules (disabled by default - enable for production)
  faultQuarantineModule:
    enabled: false
  nodeDrainerModule:
    enabled: false
  faultRemediationModule:
    enabled: false
  healthEventsAnalyzer:
    enabled: false
  
  # Cloud Monitors (disabled by default)
  cspHealthMonitor:
    enabled: false
```

For detailed configuration options for each module, see the [Module Details](#-module-details) section below.

## 📦 Module Details

### 🔍 Health Monitors

### GPU Health Monitor
**Purpose**: Monitors GPU hardware health via DCGM, detecting thermal issues, ECC errors, and XID events.

**Key Configuration Options**:
```yaml
global:
  gpuHealthMonitor:
    enabled: true
    useHostNetworking: false  # Enable for direct DCGM access
  dcgm:
    service:
      endpoint: "nvidia-dcgm.gpu-operator.svc"
      port: 5555
```

**Features**:
- Real-time GPU telemetry monitoring
- XID error detection and classification
- Temperature and power monitoring
- ECC error tracking


### Syslog Health Monitor
**Purpose**: Analyzes system logs for hardware and software fault patterns.

**Key Configuration Options**:
```yaml
global:
  syslogHealthMonitor:
    enabled: true
pollingInterval: "30m"  # How often to check logs
stateFile: "/var/run/syslog_health_monitor/state.json"
securityContext:
  capabilities:
    add: ["SYSLOG", "SYS_ADMIN"]
```

**Features**:
- Journalctl integration
- Regex pattern matching
- Persistent cursor state
- Configurable lookback periods



### CSP Health Monitor
**Purpose**: Integrates with cloud service provider APIs for maintenance events and health notifications.

**Key Configuration Options**:
```yaml
global:
  cspHealthMonitor:
    enabled: false
cspName: "gcp"  # or "aws"
configToml:
  maintenanceEventPollIntervalSeconds: 60
  gcp:
    targetProjectId: "your-project"
    apiPollingIntervalSeconds: 60
  aws:
    accountId: "123456789012"
    region: "us-east-1"
```

**Features**:
- GCP and AWS maintenance event detection
- Proactive node quarantine for scheduled maintenance
- Cloud provider API integration

### 🏗️ Core Modules

### Platform Connectors
**Purpose**: Receives health events from monitors via gRPC, persists them to MongoDB, and updates Kubernetes node status.

**Key Configuration Options**:
```yaml
platformConnector:
  mongodbStore:
    enabled: true
    connectionString: "mongodb://nvsentinel-mongodb:27017"
```

**Features**:
- gRPC server interface for health monitors
- Event validation and persistence
- MongoDB integration with TLS support
- Updates Kubernetes node conditions based on health events
- Creates Kubernetes events for node health status changes

### Fault Quarantine Module
**Purpose**: Watches MongoDB for health events and cordons nodes based on configurable rule sets.

**Key Configuration Options**:
```yaml
global:
  faultQuarantineModule:
    enabled: false
logLevel: 1
config: |
  label-prefix = "k8saas.nvidia.com/"
  [[rule-sets]]
    name = "GPU fatal error ruleset"
    [[rule-sets.match.all]]
      kind = "HealthEvent"
      expression = "event.isFatal == true"
    [rule-sets.cordon]
      shouldCordon = true
```

**Features**:
- Watches MongoDB change streams for new events
- TOML-based rule configuration
- Multi-condition rule evaluation
- Percentage-based cordon limits
- Label-based node management



### Node Drainer Module
**Purpose**: Watches MongoDB for cordoned nodes and gracefully evicts workloads with configurable policies.

**Key Configuration Options**:
```yaml
global:
  nodeDrainerModule:
    enabled: false
config: |
  evictionTimeoutInSeconds = "60"
  [[userNamespaces]]
  name = "runai-*"
  mode = "AllowCompletion"
```

**Features**:
- Watches MongoDB change streams for node cordon events
- Configurable eviction timeouts
- Namespace-specific drain policies
- Workload completion awareness
- Graceful termination handling



### Fault Remediation Module
**Purpose**: Watches MongoDB for drain completion events and triggers external break-fix systems.

**Key Configuration Options**:
```yaml
global:
  faultRemediationModule:
    enabled: false
maintenanceResource:
  apiGroup: "janitor.dgxc.nvidia.com"
  namespace: "dgxc-janitor"
  rebootResource:
    name: "rebootnodes"
```

**Features**:
- Watches MongoDB change streams for remediation trigger events
- Kubernetes CRD integration
- Template-based remediation workflows
- External system integration



### Health Events Analyzer
**Purpose**: Watches MongoDB change streams to analyze event patterns and generate recommended actions.

**Key Configuration Options**:
```yaml
global:
  healthEventsAnalyzer:
    enabled: false
logLevel: 1
config: |
  [[rules]]
  name = "XID13->XID31 Detection"
  time_window = "30m"
  recommended_action = "COMPONENT_RESET"
```

**Features**:
- Watches MongoDB change streams for health event patterns
- Time-window based pattern analysis
- Sequential event correlation
- Automated action recommendations
- Complex rule evaluation



### MongoDB Store
**Purpose**: Provides persistent storage for health events with real-time change streams.

**Key Configuration Options**:
```yaml
mongodb:
  architecture: replicaset
  replicaCount: 3
  auth:
    enabled: true
  tls:
    enabled: true
    mTLS:
      enabled: true
  resources:
    requests:
      cpu: 1
      memory: 1.5Gi
```

**Features**:
- High availability replica set
- TLS/mTLS encryption
- Change stream notifications
- Automatic credential management

## 📋 Requirements

- **Kubernetes**: 1.25 or later
- **Helm**: 3.0 or later
- **NVIDIA GPU Operator**: For GPU monitoring capabilities (includes DCGM)
- **Storage**: Persistent storage for MongoDB (recommended 10GB+)
- **Network**: Cluster networking for inter-service communication

## 🛠️ Development

### Project Structure

```
nvsentinel/
├── health-monitors/          # Individual health monitor implementations
├── platform-connectors/     # gRPC platform connector services
├── fault-quarantine-module/ # Node quarantine logic
├── node-drainer-module/     # Workload draining coordination
├── fault-remediation-module/# External remediation integration
├── store-client-sdk/        # MongoDB client SDK
├── distros/kubernetes/      # Helm charts and manifests
└── testcases/              # Test suites and utilities
```

### Building

NVSentinel uses a monorepo structure. Build each module independently:

```bash
# Build a Docker-based module (example: gpu-health-monitor)
cd health-monitors/gpu-health-monitor
make docker-build-dcgm3  # or docker-build-dcgm4

# Build Go modules using ko (example: platform-connectors)
cd platform-connectors
make docker-build

# Or build Go binaries directly
cd fault-quarantine-module
go build ./...

# Run tests for a module
cd health-monitors/syslog-health-monitor
go test ./...
```

## 🤝 Contributing

We welcome contributions from the community! There are many ways to get involved:

### Ways to Contribute

- 🐛 **Report bugs** - Help us identify and fix issues
- 💡 **Request features** - Share ideas for improvements
- 📝 **Improve documentation** - Make our docs clearer and more comprehensive
- 🧪 **Add tests** - Increase test coverage and reliability
- 🔧 **Fix issues** - Submit pull requests to resolve bugs
- 💬 **Help others** - Answer questions in discussions and issues

### Getting Started

1. **Read our [Contributing Guide](CONTRIBUTING.md)** for detailed guidelines
2. **Check our [Development Guide](DEVELOPMENT.md)** for setup instructions
3. **Browse [open issues](https://github.com/NVIDIA/NVSentinel/issues)** for contribution opportunities
4. **Join the [discussions](https://github.com/NVIDIA/NVSentinel/discussions)** to connect with the community

All contributors must sign their commits (DCO). See the contributing guide for details.

## 💬 Community & Support

### Get Help

- 🐛 **Bug Reports**: [Create an issue](https://github.com/NVIDIA/NVSentinel/issues/new?template=bug_report.yml)
- 💡 **Feature Requests**: [Request a feature](https://github.com/NVIDIA/NVSentinel/issues/new?template=feature_request.yml)
- ❓ **Questions**: [Start a discussion](https://github.com/NVIDIA/NVSentinel/discussions/new?category=q-a)
- 💬 **General Discussion**: [Join our discussions](https://github.com/NVIDIA/NVSentinel/discussions)
- 🔒 **Security Issues**: See our [Security Policy](SECURITY.md)

### Stay Connected

- ⭐ **Star this repository** to show your support
- 👀 **Watch** for updates on releases and announcements
- 🔗 **Share** NVSentinel with others who might benefit

## 📄 License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

---

*Built with ❤️ by NVIDIA for GPU infrastructure reliability*
