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

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

export CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-demo}"
NAMESPACE="nvsentinel"

log() {
    echo "[$(date +'%Y-%m-%d %H:%M:%S')] $*"
}

section() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  $*"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo ""
}

check_prerequisites() {
    section "Checking Prerequisites"

    local missing=()

    command -v docker &>/dev/null || missing+=("docker")
    command -v kind &>/dev/null || missing+=("kind")
    command -v kubectl &>/dev/null || missing+=("kubectl")
    command -v helm &>/dev/null || missing+=("helm")
    command -v ko &>/dev/null || missing+=("ko")
    command -v go &>/dev/null || missing+=("go")
    command -v jq &>/dev/null || missing+=("jq")

    if [ ${#missing[@]} -gt 0 ]; then
        log "❌ Missing required tools: ${missing[*]}"
        log "Please install them and try again"
        exit 1
    fi

    log "✓ All prerequisites found"
}

create_kind_cluster() {
    section "Phase 1: Creating KIND Cluster"

    if kind get clusters | grep -q "^${CLUSTER_NAME}$"; then
        log "Cluster '$CLUSTER_NAME' already exists. Deleting it first..."
        kind delete cluster --name "$CLUSTER_NAME"
    fi

    log "Creating KIND cluster with 2 nodes..."
    cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
- role: worker
EOF

    log "✓ KIND cluster created"
}

install_cert_manager() {
    section "Phase 2: Installing cert-manager"

    log "Installing cert-manager..."
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml

    log "Waiting for cert-manager to be ready..."
    kubectl wait --for=condition=available --timeout=300s \
        deployment/cert-manager -n cert-manager
    kubectl wait --for=condition=available --timeout=300s \
        deployment/cert-manager-webhook -n cert-manager
    kubectl wait --for=condition=available --timeout=300s \
        deployment/cert-manager-cainjector -n cert-manager

    log "✓ cert-manager installed"
}

install_nvsentinel() {
    section "Phase 3: Installing NVSentinel"

    local chart_version="${NVSENTINEL_CHART_VERSION:-v1.2.0}"
    local image_tag="${NVSENTINEL_IMAGE_TAG:-main-83a39fa}"

    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    local ARCH=$(uname -m)
    local mongo_repo="bitnamilegacy/mongodb"
    local mongo_tag="8.0.3-debian-12-r1"
    if [[ "$ARCH" == "arm64" || "$ARCH" == "aarch64" ]]; then
        mongo_repo="dlavrenuek/bitnami-mongodb-arm"
        mongo_tag="8.0.4"
    fi

    log "Installing NVSentinel Helm chart ${chart_version} from OCI registry..."
    log "Using image tag: ${image_tag} (contains custom remediation action support)"
    log "MongoDB image: ${mongo_repo}:${mongo_tag} (${ARCH})"

    helm upgrade --install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
        --version "$chart_version" \
        --namespace "$NAMESPACE" \
        --values "$SCRIPT_DIR/../config/nvsentinel-values.yaml" \
        --set global.image.tag="${image_tag}" \
        --set "mongodb-store.mongodb.image.repository=${mongo_repo}" \
        --set "mongodb-store.mongodb.image.tag=${mongo_tag}" \
        --wait --timeout=10m

    log "✓ NVSentinel installed"

    log "Granting fault-remediation RBAC for MemoryReclaim CRD..."
    cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: nvsentinel-memoryreclaim
rules:
- apiGroups: ["demo.nvsentinel.nvidia.com"]
  resources: ["memoryreclaims"]
  verbs: ["create", "get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: nvsentinel-memoryreclaim
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: nvsentinel-memoryreclaim
subjects:
- kind: ServiceAccount
  name: fault-remediation
  namespace: $NAMESPACE
EOF
    log "✓ RBAC for MemoryReclaim CRD granted"
}

build_and_load_images() {
    section "Phase 4: Building Custom Component Images"

    local demo_dir="$PROJECT_ROOT/demos/local-custom-remediation-demo"

    log "Building memory-pressure-monitor..."
    docker build -t memory-pressure-monitor:demo \
        -f "$demo_dir/memory-pressure-monitor/Dockerfile" \
        "$PROJECT_ROOT"
    kind load docker-image memory-pressure-monitor:demo --name "$CLUSTER_NAME"

    log "Building memory-reclaim-controller..."
    docker build -t memory-reclaim-controller:demo \
        -f "$demo_dir/memory-reclaim-controller/Dockerfile" \
        "$PROJECT_ROOT"
    kind load docker-image memory-reclaim-controller:demo --name "$CLUSTER_NAME"

    log "✓ All component images built and loaded"
}

deploy_memory_pressure_monitor() {
    section "Phase 5: Deploying Memory Pressure Monitor"

    local worker_node="${CLUSTER_NAME}-worker"
    local avail_kb
    avail_kb=$(docker exec "$worker_node" awk '/MemAvailable/ {print $2}' /proc/meminfo 2>/dev/null || echo "0")
    local avail_mb=$((avail_kb / 1024))
    MEM_THRESHOLD_MB=${MEM_THRESHOLD_MB:-$((avail_mb - 200))}

    log "Worker node available memory: ${avail_mb} MB"
    log "Setting threshold to ${MEM_THRESHOLD_MB} MB (triggers when stress pod consumes ~200+ MB)"

    log "Deploying memory-pressure-monitor DaemonSet..."
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: memory-pressure-monitor
  namespace: $NAMESPACE
  labels:
    app: memory-pressure-monitor
spec:
  selector:
    matchLabels:
      app: memory-pressure-monitor
  template:
    metadata:
      labels:
        app: memory-pressure-monitor
    spec:
      serviceAccountName: default
      containers:
      - name: monitor
        image: memory-pressure-monitor:demo
        imagePullPolicy: Never
        env:
        - name: NODE_NAME
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: MEM_THRESHOLD_MB
          value: "${MEM_THRESHOLD_MB}"
        - name: PROCFS_PATH
          value: "/host/proc/meminfo"
        - name: POLL_INTERVAL_SECONDS
          value: "10"
        - name: SOCKET_PATH
          value: "/var/run/nvsentinel.sock"
        volumeMounts:
        - name: host-proc
          mountPath: /host/proc
          readOnly: true
        - name: nvsentinel-socket
          mountPath: /var/run
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            cpu: 200m
            memory: 128Mi
      volumes:
      - name: host-proc
        hostPath:
          path: /proc
      - name: nvsentinel-socket
        hostPath:
          path: /var/run/nvsentinel
          type: DirectoryOrCreate
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
            - matchExpressions:
              - key: node-role.kubernetes.io/control-plane
                operator: DoesNotExist
      tolerations:
      - operator: Exists
EOF

    log "✓ Memory pressure monitor deployed"
}

deploy_memory_reclaim_controller() {
    section "Phase 6: Deploying Memory Reclaim Controller"

    log "Installing MemoryReclaim CRD..."
    kubectl apply -f "$SCRIPT_DIR/../config/memoryreclaim-crd.yaml"

    log "Creating RBAC for memory-reclaim-controller..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ServiceAccount
metadata:
  name: memory-reclaim-controller
  namespace: $NAMESPACE
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: memory-reclaim-controller
rules:
- apiGroups: ["demo.nvsentinel.nvidia.com"]
  resources: ["memoryreclaims"]
  verbs: ["get", "list", "watch", "update"]
- apiGroups: ["demo.nvsentinel.nvidia.com"]
  resources: ["memoryreclaims/status"]
  verbs: ["get", "update"]
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "delete"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: memory-reclaim-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: memory-reclaim-controller
subjects:
- kind: ServiceAccount
  name: memory-reclaim-controller
  namespace: $NAMESPACE
EOF

    log "Deploying memory-reclaim-controller..."
    cat <<EOF | kubectl apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: memory-reclaim-controller
  namespace: $NAMESPACE
  labels:
    app: memory-reclaim-controller
spec:
  replicas: 1
  selector:
    matchLabels:
      app: memory-reclaim-controller
  template:
    metadata:
      labels:
        app: memory-reclaim-controller
    spec:
      serviceAccountName: memory-reclaim-controller
      containers:
      - name: controller
        image: memory-reclaim-controller:demo
        imagePullPolicy: Never
        env:
        - name: NAMESPACE
          value: "default"
        resources:
          requests:
            cpu: 50m
            memory: 64Mi
          limits:
            cpu: 200m
            memory: 128Mi
EOF

    kubectl wait --for=condition=available --timeout=120s \
        deployment/memory-reclaim-controller -n "$NAMESPACE"

    log "✓ Memory reclaim controller deployed"
}

wait_for_pods() {
    section "Phase 8: Waiting for All Pods"

    log "Waiting for all pods to be ready (this may take 2-3 minutes)..."

    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=nvsentinel \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            log "WARNING: Platform Connectors not ready yet"
        }

    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=fault-quarantine \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            log "WARNING: Fault Quarantine not ready yet"
        }

    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=mongodb \
        -n "$NAMESPACE" \
        --timeout=300s > /dev/null 2>&1 || {
            log "WARNING: MongoDB not ready yet"
        }

    kubectl wait --for=condition=ready pod \
        -l app=memory-pressure-monitor \
        -n "$NAMESPACE" \
        --timeout=120s > /dev/null 2>&1 || {
            log "WARNING: Memory pressure monitor not ready yet"
        }

    kubectl wait --for=condition=available --timeout=120s \
        deployment/memory-reclaim-controller -n "$NAMESPACE" > /dev/null 2>&1 || {
            log "WARNING: Memory reclaim controller not ready yet"
        }

    log "✓ All pods are ready"
}

show_summary() {
    section "Setup Complete!"

    cat <<EOF

✅ Custom Remediation Demo Ready!

📊 Cluster Information:
   • Cluster: $CLUSTER_NAME
   • NVSentinel Namespace: $NAMESPACE

🔧 Components Deployed:
   ✓ KIND Cluster (1 control-plane + 1 worker)
   ✓ cert-manager
   ✓ NVSentinel (via local Helm chart):
     - Platform Connectors
     - Fault Quarantine
     - Node Drainer
     - Fault Remediation (with RECLAIM_MEMORY action)
     - MongoDB
   ✓ Custom Components:
     - Memory Pressure Monitor (DaemonSet on worker)
     - Memory Reclaim Controller (Deployment)

📝 Next Steps:
   1. View cluster status:
      make show-cluster

   2. Trigger memory pressure:
      make trigger

   3. Verify the remediation workflow:
      make verify

🧹 Cleanup:
   make cleanup

EOF
}

main() {
    log "Starting Custom Remediation Demo setup..."
    echo ""

    check_prerequisites
    create_kind_cluster
    install_cert_manager
    install_nvsentinel
    build_and_load_images
    deploy_memory_pressure_monitor
    deploy_memory_reclaim_controller
    wait_for_pods
    show_summary

    log "✅ Setup complete!"
}

main "$@"
