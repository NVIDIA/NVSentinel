#!/bin/bash
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -e

# Colors for output
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

CLUSTER_NAME="nvsentinel-demo"
NAMESPACE="nvsentinel"

log() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

check_prerequisites() {
    log "Checking prerequisites..."
    
    local missing=()
    
    if ! command -v docker &> /dev/null; then
        missing+=("docker")
    fi
    
    if ! command -v kind &> /dev/null; then
        missing+=("kind")
    fi
    
    if ! command -v kubectl &> /dev/null; then
        missing+=("kubectl")
    fi
    
    if ! command -v helm &> /dev/null; then
        missing+=("helm")
    fi
    
    if ! command -v jq &> /dev/null; then
        missing+=("jq")
    fi
    
    if [ ${#missing[@]} -ne 0 ]; then
        error "Missing required tools: ${missing[*]}\nPlease install them and try again. See README.md for installation links."
    fi
    
    success "All prerequisites found"
}

create_cluster() {
    log "Creating KIND cluster: $CLUSTER_NAME"
    
    # Check if cluster already exists
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        warn "Cluster '$CLUSTER_NAME' already exists. Deleting it first..."
        kind delete cluster --name "$CLUSTER_NAME"
    fi
    
    # Create cluster with 1 worker node (minimal config to save disk space)
    cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
EOF
    
    success "Cluster created successfully"
    
    # Set kubectl context
    kubectl config use-context "kind-${CLUSTER_NAME}"
    
    # Wait for nodes to be ready
    log "Waiting for nodes to be ready..."
    kubectl wait --for=condition=ready nodes --all --timeout=120s
    
    success "All nodes are ready"
}

install_cert_manager() {
    log "Installing cert-manager..."
    
    helm repo add jetstack https://charts.jetstack.io --force-update > /dev/null 2>&1
    
    helm upgrade --install cert-manager jetstack/cert-manager \
        --namespace cert-manager \
        --create-namespace \
        --version v1.16.2 \
        --set crds.enabled=true \
        --wait \
        --timeout 5m > /dev/null 2>&1
    
    success "cert-manager installed"
}

install_prometheus_crds() {
    log "Installing Prometheus CRDs (for PodMonitor support)..."
    
    # Install only the CRDs, not the full Prometheus operator
    if kubectl apply --server-side -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.68.0/example/prometheus-operator-crd/monitoring.coreos.com_podmonitors.yaml; then
        success "Prometheus CRDs installed"
    else
        error "Failed to install Prometheus CRDs"
        return 1
    fi
}

install_nvsentinel() {
    log "Installing NVSentinel (minimal configuration)..."
    
    # Create namespace
    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - > /dev/null
    
    # Create a minimal values file for this demo
    cat > /tmp/nvsentinel-demo-values.yaml <<EOF
# Minimal NVSentinel configuration for local demo
global:
  dryRun: false
  
  # Enable GPU health monitor for Error detection (works with fake DCGM)
  gpuHealthMonitor:
    enabled: true
  syslogHealthMonitor:
    enabled: false
  cspHealthMonitor:
    enabled: false
  kubernetesObjectMonitor:
    enabled: false
  
  # Enable only core components needed for demo
  platformConnector:
    enabled: true
    replicas: 1
  
  faultQuarantine:
    enabled: true
    replicas: 1
  
  # Disable other components
  nodeDrainer:
    enabled: false
  faultRemediation:
    enabled: false
  healthEventsAnalyzer:
    enabled: false
  janitor:
    enabled: false
  labeler:
    enabled: false  # Disabled to prevent removing our demo labels
  metadataCollector:
    enabled: false
  
  # Enable MongoDB for event storage
  mongodbStore:
    enabled: true

# Disable monitoring (no Prometheus in this demo)
monitoring:
  enabled: false

# MongoDB Store configuration - copied from values-tilt.yaml (lines 73-184)
mongodb-store:
  useBitnami: true
  usePerconaOperator: false
  
  job:
    nodeSelector:
      node-role.kubernetes.io/control-plane: ""
    tolerations:
      - operator: Exists
  
  # Bitnami configuration
  mongodb:
    replicaCount: 1
    nodeSelector:
      node-role.kubernetes.io/control-plane: ""
    
    tolerations:
    - operator: Exists

    jobTolerations:
    - operator: Exists

    image:
      registry: "docker.io"
      repository: "bitnamilegacy/mongodb"
      tag: "8.0.3-debian-12-r1"
      pullPolicy: "IfNotPresent"

    tls:
      replicaset:
        existingSecrets:
          - "mongo-server-cert-0"
      
      image:
        registry: "docker.io"
        repository: "bitnamilegacy/nginx"
        tag: "1.27.2-debian-12-r2"
        pullPolicy: "IfNotPresent"

    metrics:
      enabled: true
      image:
        registry: docker.io
        repository: bitnamilegacy/mongodb-exporter
        tag: 0.41.2-debian-12-r1
    
    # Demo-specific overrides
    auth:
      enabled: true
      rootPassword: "demo-password"
    persistence:
      enabled: false

# Platform Connector configuration
platformConnector:
  resources:
    requests:
      memory: "128Mi"
      cpu: "100m"
    limits:
      memory: "256Mi"
      cpu: "500m"

# Fault Quarantine configuration
faultQuarantine:
  resources:
    requests:
      memory: "128Mi"
      cpu: "100m"
    limits:
      memory: "256Mi"
      cpu: "500m"

# Simple Health Client for injecting test events
simpleHealthClient:
  enabled: true
  replicas: 1
  resources:
    requests:
      memory: "64Mi"
      cpu: "50m"
    limits:
      memory: "128Mi"
      cpu: "200m"
EOF
    
    # Install NVSentinel from OCI registry (uses published images)
    # For latest development code, build and load images into KIND first
    local nvsentinel_version="${NVSENTINEL_VERSION:-v0.4.1}"
    
    log "Installing NVSentinel ${nvsentinel_version} from OCI registry..."
    log "(Set NVSENTINEL_VERSION env var to use a different version)"
    log "This includes MongoDB pod (single-member replica set for change streams)..."
    log "This will take ~1-2 minutes for MongoDB to initialize"
    
    helm upgrade --install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
        --version "$nvsentinel_version" \
        --namespace "$NAMESPACE" \
        --values /tmp/nvsentinel-demo-values.yaml \
        --wait \
        --timeout 10m
    
    success "NVSentinel installed"
}

deploy_fake_dcgm() {
    log "Deploying fake DCGM for GPU simulation..."
    
    # Create gpu-operator namespace
    kubectl create namespace gpu-operator --dry-run=client -o yaml | kubectl apply -f - > /dev/null
    
    # Deploy fake DCGM daemonset (from Tilt setup)
    # NOTE: We don't use nodeSelector here to avoid race condition with labeling
    # Instead, we deploy on all worker nodes first, then label them
    cat <<EOF | kubectl apply -f - > /dev/null
apiVersion: apps/v1
kind: DaemonSet
metadata:
  labels:
    app: nvidia-dcgm
  name: nvidia-dcgm
  namespace: gpu-operator
spec:
  selector:
    matchLabels:
      app: nvidia-dcgm
  template:
    metadata:
      labels:
        app: nvidia-dcgm
    spec:
      containers:
      - image: ghcr.io/nvidia/nvsentinel-fake-dcgm:4.2.0
        name: nvidia-dcgm-ctr
        command: ["/bin/bash", "-c"]
        args:
          - |
            nv-hostengine -n -b ALL &
            sleep infinity
        readinessProbe:
          exec:
            command:
            - /bin/bash
            - -c
            - "timeout 1 bash -c 'cat < /dev/null > /dev/tcp/127.0.0.1/5555'"
          initialDelaySeconds: 3
          periodSeconds: 2
          timeoutSeconds: 1
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: node-role.kubernetes.io/control-plane
                operator: DoesNotExist
      tolerations:
      - key: nvidia.com/gpu
        operator: Exists
---
apiVersion: v1
kind: Service
metadata:
  labels:
    app: nvidia-dcgm
  name: nvidia-dcgm
  namespace: gpu-operator
spec:
  type: ClusterIP
  ports:
  - name: dcgm
    port: 5555
    protocol: TCP
    targetPort: 5555
  selector:
    app: nvidia-dcgm
EOF
    
    # Wait for fake DCGM to be ready before continuing
    # The readiness probe checks if port 5555 is actually listening
    log "Waiting for fake DCGM to be ready..."
    kubectl wait --for=condition=ready pod \
        -l app=nvidia-dcgm \
        -n gpu-operator \
        --timeout=120s > /dev/null 2>&1
    
    success "Fake DCGM deployed and ready (port 5555 is listening)"
}

label_demo_nodes() {
    log "Labeling worker nodes for GPU simulation..."
    
    # Label worker nodes so DCGM and GPU health monitor pods schedule
    # These labels simulate having NVIDIA GPUs, drivers, and DCGM 4.x installed
    for node in $(kubectl get nodes -o name | grep worker); do
        node_name=$(echo "$node" | cut -d'/' -f2)
        kubectl label "$node" \
            nvidia.com/gpu.present=true \
            nvsentinel.dgxc.nvidia.com/driver.installed=true \
            nvsentinel.dgxc.nvidia.com/kata.enabled=false \
            nvsentinel.dgxc.nvidia.com/dcgm.version=4.x \
            --overwrite > /dev/null 2>&1
        log "  Labeled $node_name"
    done
    
    success "Nodes labeled for demo"
}

wait_for_pods() {
    log "Waiting for all pods to be ready (this may take 2-3 minutes)..."
    
    # Wait for platform-connectors
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=nvsentinel \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            error "Platform Connectors failed to start"
            return 1
        }
    
    # Wait for fault-quarantine
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=fault-quarantine \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            error "Fault Quarantine failed to start"
            return 1
        }
    
    # Wait for mongodb
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=mongodb \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            error "MongoDB failed to start"
            return 1
        }
    
    # Wait for gpu-health-monitor (should start after we label nodes)
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=gpu-health-monitor \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            error "GPU Health Monitor failed to start"
            return 1
        }
    
    # Note: Fake DCGM is already waited for in deploy_fake_dcgm()
    
    success "All pods are ready"
}

print_status() {
    echo ""
    echo "=========================================="
    echo "  NVSentinel Demo Environment Ready! 🎉"
    echo "=========================================="
    echo ""
    echo "Cluster: $CLUSTER_NAME"
    echo "Namespace: $NAMESPACE"
    echo ""
    echo "Nodes:"
    kubectl get nodes
    echo ""
    echo "NVSentinel Pods:"
    kubectl get pods -n "$NAMESPACE"
    echo ""
    echo "=========================================="
    echo ""
    success "Setup complete! Run './scripts/01-show-cluster.sh' to view cluster state"
    echo ""
}

main() {
    log "Starting NVSentinel demo setup..."
    echo ""
    
    check_prerequisites
    create_cluster
    install_cert_manager
    install_prometheus_crds
    install_nvsentinel
    deploy_fake_dcgm
    label_demo_nodes
    wait_for_pods
    print_status
}

main "$@"

