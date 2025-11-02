# NVSentinel Helm Chart

[![Helm](https://img.shields.io/badge/Helm-3.0+-0F1689.svg?logo=helm&logoColor=white)](https://helm.sh/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5.svg?logo=kubernetes&logoColor=white)](https://kubernetes.io/)

Official Helm chart for NVSentinel - GPU node resilience system for automated fault detection, quarantine, and remediation.

## Quick Start

### Prerequisites

- **Kubernetes**: 1.25 or later
- **Helm**: 3.0 or later
- **NVIDIA GPU Operator**: Required for GPU monitoring (includes DCGM)
- **cert-manager**: Required for TLS certificate management (v1.19.0+)
- **Persistent Storage**: For MongoDB (default: 10GB per replica)

```bash
# Install cert-manager
helm repo add jetstack https://charts.jetstack.io --force-update
helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --version v1.19.1 \
  --set installCRDs=true

# Install Prometheus (optional, for metrics collection)
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install prometheus prometheus-community/kube-prometheus-stack \
  --namespace monitoring \
  --create-namespace
```

### Installing/Upgrading NVSentinel 

```bash
# Install with default configuration
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --version v0.2.0 \
  --namespace nvsentinel \
  --create-namespace

# Upgrade to a new version
helm upgrade nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --version v0.3.0 \
  --namespace nvsentinel

# Uninstall
helm uninstall nvsentinel --namespace nvsentinel
```

## Chart Structure

The NVSentinel Helm chart follows a modular architecture with independent subcharts:

```
nvsentinel/
├── Chart.yaml                      # Main chart metadata
├── values.yaml                     # Default configuration
├── values-full.yaml                # Complete reference with all options
├── charts/                         # Subcharts for each module
│   ├── platform-connectors/        # Core: gRPC server & event persistence
│   ├── gpu-health-monitor/         # Health: GPU monitoring via DCGM
│   ├── syslog-health-monitor/      # Health: System log analysis
│   ├── csp-health-monitor/         # Health: Cloud provider events
│   ├── fault-quarantine/           # Core: Node cordoning & tainting
│   ├── node-drainer/               # Core: Workload eviction
│   ├── fault-remediation/          # Core: Maintenance CRD creation
│   ├── janitor/                    # Core: Cloud provider API integration
│   ├── health-events-analyzer/     # Analysis: Event pattern detection
│   ├── labeler/                    # Utility: Node labeling
│   ├── mongodb-store/              # Storage: Event database
│   └── incluster-file-server/      # Utility: Log collection storage
└── templates/                      # Main chart templates
```

## Configuration

### Configuration Files

- **`values.yaml`**: Default configuration (monitoring only)
- **`values-full.yaml`**: Complete reference with all available options and detailed documentation
- **`values-tilt.yaml`**: Development configuration for Tilt
- **`values-tilt-arm64.yaml`**: ARM64-specific overrides

### Global Configuration

```yaml
global:
  # Image tag for all components
  image:
    tag: "main"

  # Dry-run mode - log actions without executing them
  dryRun: false

  # Module enable/disable flags
  gpuHealthMonitor:
    enabled: true
  syslogHealthMonitor:
    enabled: true
  faultQuarantine:
    enabled: false
  nodeDrainer:
    enabled: false
  faultRemediation:
    enabled: false
  janitor:
    enabled: false
  mongodbStore:
    enabled: false
```

### Module-Specific Configuration

#### Fault Quarantine

Configure quarantine rules using CEL expressions:

```yaml
fault-quarantine:
  # Label prefix for node labels and annotations
  labelPrefix: "k8saas.nvidia.com/"

  # Circuit breaker to prevent mass quarantines
  circuitBreaker:
    enabled: true
    percentage: 50    # Max % of nodes to quarantine
    duration: "5m"    # Cooldown period

  # Quarantine rules
  ruleSets:
    - version: "1"
      name: "GPU fatal error ruleset"
      match:
        all:
          - kind: "HealthEvent"
            expression: "event.agent == 'gpu-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
          - kind: "Node"
            expression: |
              !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
      cordon:
        shouldCordon: true
      # Optional taint
      # taint:
      #   key: "nvidia.com/gpu-error"
      #   value: "fatal"
      #   effect: "NoSchedule"
```

#### Node Drainer

Configure eviction strategies per namespace:

```yaml
node-drainer:
  # Eviction timeout in seconds
  evictionTimeoutInSeconds: "60"
  
  # System namespaces to skip
  systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator|skyhook)$"
  
  # Force delete timeout (minutes)
  deleteAfterTimeoutMinutes: 60
  
  # NotReady pod timeout (minutes)
  notReadyTimeoutMinutes: 5

  # Per-namespace eviction strategies
  userNamespaces:
    - name: "*"
      mode: "AllowCompletion"  # Options: "Immediate", "AllowCompletion", "DeleteAfterTimeout"
```

#### Fault Remediation

Configure maintenance resource creation and log collection:

```yaml
fault-remediation:
  # Maintenance resource configuration
  maintenance:
    apiGroup: "janitor.dgxc.nvidia.com"
    version: "v1alpha1"
    namespace: "nvsentinel"
    resourceNames:
      - "rebootnodes"
      - "terminatenodes"
    
    # Go template for maintenance resources
    template: |
      apiVersion: janitor.dgxc.nvidia.com/v1alpha1
      kind: RebootNode
      metadata:
        name: maintenance-{{ .NodeName }}-{{ .HealthEventID }}
      spec:
        nodeName: {{ .NodeName }}

  # Retry configuration for node updates
  updateRetry:
    maxRetries: 5
    retryDelaySeconds: 10

  # Log collection (optional)
  logCollector:
    enabled: false
    image:
      repository: ghcr.io/nvidia/nvsentinel/log-collector
      pullPolicy: IfNotPresent
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    gpuOperatorNamespaces: "gpu-operator"
    enableGcpSosCollection: false
    enableAwsSosCollection: false
```

#### Janitor

Configure cloud provider integration:

```yaml
janitor:
  serviceAccount:
    create: true
  
  config:
    timeout: "25m"
    manualMode: false
    httpPort: 8082
    
    # Node exclusions
    nodes:
      exclusions: []
    
    # Controller configuration
    controllers:
      rebootNode:
        enabled: true
        timeout: "25m"
      terminateNode:
        enabled: true
        timeout: "25m"

  # Cloud provider configuration
  csp:
    provider: "kind"  # Options: kind, kwok, aws, gcp, azure, oci
```

##### AWS Configuration

```yaml
janitor:
  csp:
    provider: "aws"
    aws:
      region: "us-east-1"
      accountId: "123456789012"
      iamRoleName: "nvsentinel-janitor-role"
```

##### GCP Configuration

```yaml
janitor:
  csp:
    provider: "gcp"
    gcp:
      project: "my-project"
      zone: "us-central1-a"
      serviceAccount: "nvsentinel-janitor"
```

##### Azure Configuration

```yaml
janitor:
  csp:
    provider: "azure"
    azure:
      subscriptionId: "12345678-1234-1234-1234-123456789012"
      resourceGroup: ""
      location: ""
      clientId: "12345678-1234-1234-1234-123456789012"
```

##### OCI Configuration

```yaml
janitor:
  csp:
    provider: "oci"
    oci:
      region: "us-ashburn-1"
      compartment: "ocid1.compartment.oc1..aaa..."
      credentialsFile: ""
      profile: "DEFAULT"
      principalId: "ocid1.principal.oc1..aaa..."
```

### Complete Configuration Reference

For detailed documentation of all available configuration options, see:

- **[`values-full.yaml`](nvsentinel/values-full.yaml)**: Complete reference with detailed comments and examples
- **[`values.yaml`](nvsentinel/values.yaml)**: Default values used by the chart

To view all options from the published chart:

```bash
helm show values oci://ghcr.io/nvidia/nvsentinel --version v0.2.0
```
