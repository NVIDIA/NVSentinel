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
SLINKY_NAMESPACE="slinky"

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
    
    command -v kind &>/dev/null || missing+=("kind")
    command -v kubectl &>/dev/null || missing+=("kubectl")
    command -v docker &>/dev/null || missing+=("docker")
    command -v helm &>/dev/null || missing+=("helm")
    command -v ko &>/dev/null || missing+=("ko")
    command -v go &>/dev/null || missing+=("go")
    
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

prepare_namespace() {
    section "Phase 3: Preparing Namespace"
    
    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    
    log "Creating custom drain template ConfigMap..."
    cat <<'EOF' | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: slinky-drain-template
  namespace: nvsentinel
data:
  drain-template.yaml: |
    apiVersion: nvsentinel.nvidia.com/v1alpha1
    kind: DrainRequest
    spec:
      nodeName: {{ .HealthEvent.NodeName }}
      checkName: {{ .HealthEvent.CheckName }}
      recommendedAction: {{ .HealthEvent.RecommendedAction.String }}
      errorCode:
      {{- range .HealthEvent.ErrorCode }}
      - "{{ . }}"
      {{- end }}
      healthEventID: {{ .EventID }}
      entitiesImpacted:
      {{- range .HealthEvent.EntitiesImpacted }}
      - type: {{ .EntityType }}
        value: "{{ .EntityValue }}"
      {{- end }}
      reason: "{{ .HealthEvent.Message }}"
EOF
    
    log "✓ Namespace and drain template ready"
}

install_custom_drain_crd() {
    section "Phase 4: Installing Custom Drain CRD"
    
    local crd_file="$PROJECT_ROOT/plugins/slinky-drainer/config/crd/nvsentinel.nvidia.com_drainrequests.yaml"
    
    # Generate CRD if it doesn't exist
    if [ ! -f "$crd_file" ]; then
        log "CRD file not found, generating it..."
        cd "$PROJECT_ROOT/plugins/slinky-drainer"
        make generate
        log "✓ CRD generated"
    else
        log "CRD file already exists, skipping generation"
    fi
    
    log "Installing DrainRequest CRD (required by node-drainer)..."
    kubectl apply -f "$crd_file"
    
    log "✓ DrainRequest CRD installed"
}

install_nvsentinel() {
    section "Phase 5: Installing NVSentinel"
    
    local nvsentinel_version="${NVSENTINEL_VERSION:-v0.6.0}"
    
    log "Installing Prometheus Operator CRDs (for PodMonitor)..."
    kubectl apply --server-side -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/v0.68.0/example/prometheus-operator-crd/monitoring.coreos.com_podmonitors.yaml
    
    log "Installing NVSentinel Helm chart from local directory..."
    log "Using image tag: ${nvsentinel_version}"
    
    helm upgrade --install nvsentinel \
        "$PROJECT_ROOT/distros/kubernetes/nvsentinel" \
        --namespace "$NAMESPACE" \
        --values "$SCRIPT_DIR/../config/nvsentinel-values.yaml" \
        --set global.image.tag="${nvsentinel_version}" \
        --wait --timeout=10m
    
    log "✓ NVSentinel installed"
}

check_ko() {
    if ! command -v ko &>/dev/null; then
        log "ko not found, installing..."
        go install github.com/google/ko@latest
        export PATH="$PATH:$(go env GOPATH)/bin"
    fi
}

build_and_load_plugin_images() {
    section "Phase 6: Building Custom Plugin Images"
    
    check_ko
      
    log "Building slinky-drainer..."
    cd "$PROJECT_ROOT/plugins/slinky-drainer"
    KO_DOCKER_REPO=ko.local ko build --bare --local . --tags demo
    docker tag ko.local:demo slinky-drainer:demo
    kind load docker-image slinky-drainer:demo --name "$CLUSTER_NAME"
    
    log "Building mock-slurm-operator..."
    cd "$PROJECT_ROOT/plugins/mock-slurm-operator"
    KO_DOCKER_REPO=ko.local ko build --bare --local . --tags demo
    docker tag ko.local:demo mock-slurm-operator:demo
    kind load docker-image mock-slurm-operator:demo --name "$CLUSTER_NAME"
    
    log "✓ All plugin images built and loaded"
}

create_slinky_namespace() {
    section "Phase 7: Creating Slinky Namespace"
    
    kubectl create namespace "$SLINKY_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    
    log "✓ Slinky namespace created"
}

deploy_slinky_drainer() {
    section "Phase 8: Deploying Slinky Drainer Plugin"
    
    log "Deploying Slinky Drainer..."
    cd "$PROJECT_ROOT/plugins/slinky-drainer"
    kubectl apply -k config/default
    
    kubectl set image deployment/slinky-drainer \
        controller=slinky-drainer:demo \
        -n "$NAMESPACE"
    
    kubectl patch deployment slinky-drainer \
        -n "$NAMESPACE" \
        --type=json \
        -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'
    
    kubectl wait --for=condition=available --timeout=120s \
        deployment/slinky-drainer -n "$NAMESPACE"
    
    log "✓ Slinky drainer deployed"
}

deploy_mock_slurm() {
    section "Phase 9: Deploying Mock Slurm Operator"
    
    log "Deploying Mock Slurm Operator..."
    cd "$PROJECT_ROOT/plugins/mock-slurm-operator"
    kubectl apply -k config/default
    
    kubectl set image deployment/mock-slurm-operator \
        manager=mock-slurm-operator:demo \
        -n "$NAMESPACE"
    
    kubectl patch deployment mock-slurm-operator \
        -n "$NAMESPACE" \
        --type=json \
        -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'
    
    kubectl wait --for=condition=available --timeout=120s \
        deployment/mock-slurm-operator -n "$NAMESPACE"
    
    log "✓ Mock Slurm operator deployed"
}

deploy_simple_health_client() {
    section "Phase 10: Deploying Simple Health Client"

    log "Building simple-health-client image..."
    cd "$PROJECT_ROOT/tilt/simple-health-client"
    docker build -t simple-health-client:demo -f Dockerfile "$PROJECT_ROOT"
    kind load docker-image simple-health-client:demo --name "$CLUSTER_NAME"

    log "Deploying simple-health-client..."
    kubectl apply -f deployment.yaml

    kubectl set image deployment/simple-health-client \
        simple-health-client=simple-health-client:demo \
        -n "$NAMESPACE"

    kubectl patch deployment simple-health-client \
        -n "$NAMESPACE" \
        --type=json \
        -p='[{"op": "replace", "path": "/spec/template/spec/containers/0/imagePullPolicy", "value": "Never"}]'

    kubectl wait --for=condition=available --timeout=120s \
        deployment/simple-health-client -n "$NAMESPACE"

    log "✓ Simple health client deployed"
}

create_test_workloads() {
    section "Phase 11: Creating Test Workloads"
    
    log "Creating test pods in slinky namespace..."
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: slinky-workload-1
  namespace: $SLINKY_NAMESPACE
  labels:
    app: slinky-workload
spec:
  nodeSelector:
    kubernetes.io/hostname: ${CLUSTER_NAME}-worker
  containers:
  - name: nginx
    image: public.ecr.aws/docker/library/nginx:alpine
    resources:
      requests:
        cpu: 100m
        memory: 128Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: slinky-workload-2
  namespace: $SLINKY_NAMESPACE
  labels:
    app: slinky-workload
spec:
  nodeSelector:
    kubernetes.io/hostname: ${CLUSTER_NAME}-worker
  containers:
  - name: nginx
    image: public.ecr.aws/docker/library/nginx:alpine
    resources:
      requests:
        cpu: 100m
        memory: 128Mi
EOF
    
    kubectl wait --for=condition=ready --timeout=120s \
        pod -l app=slinky-workload -n "$SLINKY_NAMESPACE"
    
    log "✓ Test workloads created"
}

show_summary() {
    section "Setup Complete!"
    
    cat <<EOF

✅ Slinky Drain Demo Ready!

📊 Cluster Information:
   • Cluster: $CLUSTER_NAME
   • NVSentinel Namespace: $NAMESPACE
   • Slinky Namespace: $SLINKY_NAMESPACE

🔧 Components Deployed:
   ✓ KIND Cluster (1 control-plane + 1 worker)
   ✓ cert-manager
   ✓ NVSentinel (via Helm chart):
     - MongoDB
     - Platform Connectors
     - Fault Quarantine
     - Node Drainer
   ✓ Custom Plugins:
     - Slinky Drainer Plugin
     - Mock Slurm Operator
   ✓ Test Workloads (2 pods on worker node)

📝 Next Steps:
   1. View cluster status:
      make show-cluster

   2. Inject a health event to trigger drain:
      make inject-health-event

   3. Verify the drain workflow:
      make verify-drain

🧹 Cleanup:
   make cleanup

EOF
}

main() {
    log "Starting Slinky Drain Demo setup..."
    echo ""
    
    check_prerequisites
    create_kind_cluster
    install_cert_manager
    prepare_namespace
    install_custom_drain_crd
    install_nvsentinel
    build_and_load_plugin_images
    create_slinky_namespace
    deploy_slinky_drainer
    deploy_mock_slurm
    deploy_simple_health_client
    create_test_workloads
    show_summary
    
    log "✅ Setup complete!"
}

main "$@"
