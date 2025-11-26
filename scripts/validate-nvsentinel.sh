#!/usr/bin/env bash

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

# NVSentinel Deployment Validator
set -uo pipefail

# Parse arguments
NAMESPACE=""
VERSION=""
VERBOSE=false
IMAGE_PATTERN=""
DATASTORE=""

usage() {
    echo "Usage: $0 --version VERSION [--namespace NAMESPACE] [--datastore DATASTORE] [--image-pattern PATTERN] [--verbose]"
    echo "  --version       Required. Expected image version (e.g., v0.0.3, tilt)"
    echo "  --namespace     Optional. Kubernetes namespace (default: nvsentinel)"
    echo "  --datastore     Optional. Datastore provider: mongodb or postgresql (default: auto-detect)"
    echo "  --image-pattern Optional. Image pattern to validate (default: ghcr.io/nvidia/nvsentinel)"
    echo "  --verbose       Optional. Print detailed image lists"
    exit 1
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --version) VERSION="$2"; shift 2 ;;
        --namespace) NAMESPACE="$2"; shift 2 ;;
        --datastore) DATASTORE="$2"; shift 2 ;;
        --image-pattern) IMAGE_PATTERN="$2"; shift 2 ;;
        --verbose) VERBOSE=true; shift ;;
        -h|--help) usage ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

# Validate required parameters
[[ -z "$VERSION" ]] && { echo "Error: --version is required"; usage; }
NAMESPACE="${NAMESPACE:-nvsentinel}"
IMAGE_PATTERN="${IMAGE_PATTERN:-ghcr.io/nvidia/nvsentinel}"

ERRORS=0

# Logging
error() { echo "✗ $1"; ((ERRORS++)); }
ok() { echo "✓ $1"; }
warn() { echo "⚠ $1"; }

# Auto-detect datastore if not specified
if [[ -z "$DATASTORE" ]]; then
    if kubectl get statefulset nvsentinel-postgresql -n "$NAMESPACE" >/dev/null 2>&1; then
        DATASTORE="postgresql"
    elif kubectl get statefulset mongodb -n "$NAMESPACE" >/dev/null 2>&1; then
        DATASTORE="mongodb"
    else
        DATASTORE="mongodb"  # Default to mongodb if neither found
    fi
else
    # Validate datastore parameter
    if [[ "$DATASTORE" != "mongodb" && "$DATASTORE" != "postgresql" ]]; then
        error "invalid datastore: $DATASTORE (must be 'mongodb' or 'postgresql')"
        exit 1
    fi
fi

# Prerequisites
command -v kubectl >/dev/null || { error "kubectl not found"; exit 1; }
kubectl cluster-info >/dev/null || { error "cluster not accessible"; exit 1; }

# Namespace
echo "=== Namespace ==="
if kubectl get ns "$NAMESPACE" >/dev/null 2>&1; then
    ok "namespace $NAMESPACE exists"
else
    error "namespace $NAMESPACE missing"
fi

# Datastore
echo "=== Datastore ==="
ok "using datastore: $DATASTORE"

# Node counts
echo "=== Cluster Nodes ==="
total_nodes=$(kubectl get nodes --no-headers 2>/dev/null | wc -l || echo "0")
total_nodes=$(echo "$total_nodes" | tr -d '\n' | tr -d ' ')

gpu_nodes=$(kubectl get nodes -l nvidia.com/gpu.present=true --no-headers 2>/dev/null | wc -l || echo "0")
gpu_nodes=$(echo "$gpu_nodes" | tr -d '\n' | tr -d ' ')

kwok_nodes=$(kubectl get nodes -l type=kwok --no-headers 2>/dev/null | wc -l || echo "0")
kwok_nodes=$(echo "$kwok_nodes" | tr -d '\n' | tr -d ' ')

ok "cluster has $total_nodes total nodes ($gpu_nodes GPU nodes, $kwok_nodes KWOK fake nodes)"

# Image versions
echo "=== Image Versions ==="
# shellcheck disable=SC2126  # wc -l is clearer than grep -c for pipeline
wrong_versions=$(kubectl get pods -n "$NAMESPACE" -o jsonpath='{.items[*].spec.containers[*].image}' 2>/dev/null | \
    tr ' ' '\n' | grep "$IMAGE_PATTERN" | grep -v ":$VERSION" | wc -l 2>/dev/null || echo "0")
wrong_versions=$(echo "$wrong_versions" | tr -d '\n' | tr -d ' ')
# shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
[[ "$wrong_versions" -eq 0 ]] && ok "all nvsentinel images use $VERSION (pattern: $IMAGE_PATTERN)" || error "$wrong_versions pods use wrong image version (pattern: $IMAGE_PATTERN)"

# Count images by registry across cluster
all_images=$( (kubectl get deployments,daemonsets,statefulsets,jobs,cronjobs,replicasets --all-namespaces -o jsonpath='{range .items[*]}{range .spec.template.spec.containers[*]}{.image}{"\n"}{end}{range .spec.template.spec.initContainers[*]}{.image}{"\n"}{end}{end}' 2>/dev/null; kubectl get pods --all-namespaces -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{range .spec.initContainers[*]}{.image}{"\n"}{end}{end}') | sort -u )

# Clean up counts by removing whitespace and ensuring numeric values
ghcr_count=$(echo "$all_images" | grep -c "ghcr.io" 2>/dev/null || echo "0")
ghcr_count=$(echo "$ghcr_count" | tr -d '\n' | tr -d ' ')

nvcr_count=$(echo "$all_images" | grep -c "nvcr.io" 2>/dev/null || echo "0")
nvcr_count=$(echo "$nvcr_count" | tr -d '\n' | tr -d ' ')

registry_k8s_count=$(echo "$all_images" | grep -c "registry.k8s.io" 2>/dev/null || echo "0")
registry_k8s_count=$(echo "$registry_k8s_count" | tr -d '\n' | tr -d ' ')

quay_count=$(echo "$all_images" | grep -c "quay.io" 2>/dev/null || echo "0")
quay_count=$(echo "$quay_count" | tr -d '\n' | tr -d ' ')

docker_count=$(echo "$all_images" | grep -c "docker.io" 2>/dev/null || echo "0")
docker_count=$(echo "$docker_count" | tr -d '\n' | tr -d ' ')

localhost_count=$(echo "$all_images" | grep -c "localhost:" 2>/dev/null || echo "0")
localhost_count=$(echo "$localhost_count" | tr -d '\n' | tr -d ' ')

# Count images from unknown registries
total_images=$(echo "$all_images" | wc -l | tr -d '\n' | tr -d ' ')
known_images=$((ghcr_count + nvcr_count + registry_k8s_count + quay_count + docker_count + localhost_count))
unknown_count=$((total_images - known_images))

# Build summary with known registries
summary="cluster images: $ghcr_count ghcr.io, $nvcr_count nvcr.io, $registry_k8s_count registry.k8s.io, $quay_count quay.io, $docker_count docker.io, $localhost_count localhost"

# Add unknown registries if any exist
if [[ "$unknown_count" -gt 0 ]]; then
    summary="$summary, $unknown_count other"
fi

ok "$summary"

if [[ "$VERBOSE" == "true" ]]; then
    echo "=== All Container Images ==="
    (kubectl get deployments,daemonsets,statefulsets,jobs,cronjobs,replicasets --all-namespaces -o jsonpath='{range .items[*]}{range .spec.template.spec.containers[*]}{.image}{"\n"}{end}{range .spec.template.spec.initContainers[*]}{.image}{"\n"}{end}{end}' 2>/dev/null; kubectl get pods --all-namespaces -o jsonpath='{range .items[*]}{range .spec.containers[*]}{.image}{"\n"}{end}{range .spec.initContainers[*]}{.image}{"\n"}{end}{end}') | sort -u | nl
fi

# Image Pull Secrets
echo "=== Image Pull Secrets ==="
secrets=("nvidia-ngcuser-pull-secret")
for secret in "${secrets[@]}"; do
    if kubectl get secret "$secret" -n "$NAMESPACE" >/dev/null 2>&1; then
        secret_type=$(kubectl get secret "$secret" -n "$NAMESPACE" -o jsonpath='{.type}' 2>/dev/null || echo "")
        if [[ "$secret_type" == "kubernetes.io/dockerconfigjson" ]]; then
            ok "$secret: present (docker registry secret)"
        else
            warn "$secret: present but wrong type ($secret_type)"
        fi
    else
        warn "$secret not found"
    fi
done

# Expected components
check_deployment() {
    local name="$1" min_replicas="$2"
    if kubectl get deployment "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
        ready=$(kubectl get deployment "$name" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        desired=$(kubectl get deployment "$name" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
        if [[ "$ready" == "$desired" && "$ready" -ge "$min_replicas" ]]; then
            ok "$name: $ready/$desired ready"
            # Check fault modules run on real nodes
            if [[ "$name" =~ (fault-quarantine|fault-remediation|node-drainer) ]]; then
                pods=$(kubectl get pods -n "$NAMESPACE" -l "app.kubernetes.io/name=${name#nvsentinel-}" -o jsonpath='{.items[*].spec.nodeName}' 2>/dev/null || echo "")
                kwok_count=$(echo "$pods" | grep -o "kwok-node" | wc -l 2>/dev/null || echo "0")
                kwok_count=$(echo "$kwok_count" | tr -d '\n' | tr -d ' ')
                if [[ "$kwok_count" -gt 0 ]]; then
                    error "$name running on kwok nodes (should be real nodes only)"
                fi
            fi
        else
            error "$name: $ready/$desired ready (expected: >=$min_replicas)"
        fi
    else
        # Check if it exists as a different resource type
        if kubectl get daemonset "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
            error "$name deployment not found (exists as daemonset)"
        elif kubectl get statefulset "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
            error "$name deployment not found (exists as statefulset)"
        else
            error "$name deployment not found"
        fi
    fi
}

# Optional components (warnings instead of errors)
check_optional_deployment() {
    local name="$1" min_replicas="$2"
    if kubectl get deployment "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
        ready=$(kubectl get deployment "$name" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        desired=$(kubectl get deployment "$name" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
        if [[ "$ready" == "$desired" && "$ready" -ge "$min_replicas" ]]; then
            ok "$name: $ready/$desired ready"
        else
            warn "$name: $ready/$desired ready (expected: >=$min_replicas)"
        fi
    else
        # Check if it exists as a different resource type
        if kubectl get daemonset "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
            warn "$name deployment not found (exists as daemonset)"
        elif kubectl get statefulset "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
            warn "$name deployment not found (exists as statefulset)"
        else
            warn "$name deployment not found (optional component)"
        fi
    fi
}

check_daemonset() {
    local name="$1"
    if kubectl get daemonset "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
        ready=$(kubectl get daemonset "$name" -n "$NAMESPACE" -o jsonpath='{.status.numberReady}' 2>/dev/null || echo "0")
        desired=$(kubectl get daemonset "$name" -n "$NAMESPACE" -o jsonpath='{.status.desiredNumberScheduled}' 2>/dev/null || echo "0")
        if [[ "$ready" == "$desired" ]]; then
            if [[ "$desired" -eq 0 ]]; then
                # Check if it has node selector that explains why no pods
                selector=$(kubectl get daemonset "$name" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.nodeSelector}' 2>/dev/null || echo "")
                if [[ -n "$selector" && "$selector" != "{}" ]]; then
                    ok "$name: 0/0 ready (node selector: no matching nodes)"
                else
                    warn "$name: 0/0 ready (no nodes scheduled)"
                fi
            else
                ok "$name: $ready/$desired ready"
            fi
        else
            error "$name: $ready/$desired ready"
        fi
    else
        error "$name not found"
    fi
}

check_statefulset() {
    local name="$1"
    if kubectl get statefulset "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
        # Automatically detect expected replicas from StatefulSet spec
        expected_replicas=$(kubectl get statefulset "$name" -n "$NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "0")
        ready=$(kubectl get statefulset "$name" -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
        # shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
        [[ "$ready" == "$expected_replicas" ]] && ok "$name: $ready/$expected_replicas ready" || error "$name: $ready/$expected_replicas ready"
    else
        error "$name not found"
    fi
}

check_job() {
    local name="$1"
    if kubectl get job "$name" -n "$NAMESPACE" >/dev/null 2>&1; then
        succeeded=$(kubectl get job "$name" -n "$NAMESPACE" -o jsonpath='{.status.succeeded}' 2>/dev/null || echo "0")
        failed=$(kubectl get job "$name" -n "$NAMESPACE" -o jsonpath='{.status.failed}' 2>/dev/null || echo "0")
        active=$(kubectl get job "$name" -n "$NAMESPACE" -o jsonpath='{.status.active}' 2>/dev/null || echo "0")

        if [[ "$succeeded" -gt 0 ]]; then
            ok "$name: completed successfully"
        elif [[ "$failed" -gt 0 ]]; then
            error "$name: failed ($failed failures)"
        elif [[ "$active" -gt 0 ]]; then
            warn "$name: still running ($active active)"
        else
            warn "$name: status unclear"
        fi
    else
        error "$name not found"
    fi
}

# Check statefulsets
echo "=== StatefulSets ==="
if [[ "$DATASTORE" == "postgresql" ]]; then
    check_statefulset "nvsentinel-postgresql"
else
    check_statefulset "mongodb"
fi

# Check jobs
echo "=== Jobs ==="
if [[ "$DATASTORE" == "mongodb" ]]; then
    check_job "create-mongodb-database"
else
    ok "no initialization jobs required for postgresql (schema auto-initialized)"
fi

# Check deployments (application components)
echo "=== Deployments ==="
check_deployment "fault-quarantine" 1
check_deployment "fault-remediation" 1
check_deployment "janitor" 1
check_deployment "labeler" 1
check_deployment "node-drainer" 1
check_optional_deployment "simple-health-client" 1

# Check daemonsets (system components that run on nodes)
echo "=== DaemonSets ==="
check_daemonset "platform-connectors"
check_daemonset "gpu-health-monitor-dcgm-3.x"
check_daemonset "gpu-health-monitor-dcgm-4.x"
check_daemonset "syslog-health-monitor-regular"
check_daemonset "syslog-health-monitor-kata"

# Pod health
echo "=== Pod Health ==="
unhealthy=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase!=Running,status.phase!=Succeeded --no-headers 2>/dev/null | wc -l || echo "0")
total=$(kubectl get pods -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l || echo "0")

if [[ "$unhealthy" -eq 0 ]]; then
    ok "all $total pods healthy"
else
    error "$unhealthy/$total pods unhealthy"
    kubectl get pods -n "$NAMESPACE" --field-selector=status.phase!=Running,status.phase!=Succeeded --no-headers 2>/dev/null || true

    # Analyze pending pods for scheduling issues
    pending_pods=$(kubectl get pods -n "$NAMESPACE" --field-selector=status.phase=Pending -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || echo "")
    if [[ -n "$pending_pods" ]]; then
        echo "=== Pending Pod Issues ==="
        for pod in $pending_pods; do
            # Check for common scheduling issues
            if kubectl get events -n "$NAMESPACE" --field-selector involvedObject.name="$pod" -o jsonpath='{.items[?(@.reason=="FailedScheduling")].message}' 2>/dev/null | grep -q "untolerated taint.*control-plane"; then
                warn "$pod: missing control-plane toleration"
            elif kubectl get events -n "$NAMESPACE" --field-selector involvedObject.name="$pod" -o jsonpath='{.items[?(@.reason=="FailedScheduling")].message}' 2>/dev/null | grep -q "didn't match.*node affinity"; then
                warn "$pod: node selector/affinity not matched"
            else
                warn "$pod: scheduling failed (check kubectl describe)"
            fi
        done
    fi
fi


# Services
echo "=== Services ==="
# Required services (datastore-specific)
if [[ "$DATASTORE" == "postgresql" ]]; then
    services=("nvsentinel-postgresql" "nvsentinel-postgresql-hl" "janitor")
else
    services=("mongodb-headless" "mongodb-metrics" "janitor")
fi

for svc in "${services[@]}"; do
    if kubectl get service "$svc" -n "$NAMESPACE" >/dev/null 2>&1; then
        endpoints=$(kubectl get endpoints "$svc" -n "$NAMESPACE" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | wc -w || echo "0")
        # shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
        [[ "$endpoints" -gt 0 ]] && ok "$svc: $endpoints endpoints" || warn "$svc: no endpoints"
    else
        warn "$svc not found"
    fi
done

# Optional services
optional_services=("simple-health-client")
for svc in "${optional_services[@]}"; do
    if kubectl get service "$svc" -n "$NAMESPACE" >/dev/null 2>&1; then
        endpoints=$(kubectl get endpoints "$svc" -n "$NAMESPACE" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null | wc -w || echo "0")
        # shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
        [[ "$endpoints" -gt 0 ]] && ok "$svc: $endpoints endpoints (optional)" || warn "$svc: no endpoints (optional)"
    else
        warn "$svc not found (optional service)"
    fi
done

# Critical issues
echo "=== Critical Issues ==="
image_pull=$(kubectl get pods -n "$NAMESPACE" -o jsonpath='{.items[?(@.status.containerStatuses[0].state.waiting.reason=="ImagePullBackOff")].metadata.name}' 2>/dev/null || echo "")
crash_loop=$(kubectl get pods -n "$NAMESPACE" -o jsonpath='{.items[?(@.status.containerStatuses[0].state.waiting.reason=="CrashLoopBackOff")].metadata.name}' 2>/dev/null || echo "")

# shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
[[ -z "$image_pull" ]] && ok "no ImagePullBackOff issues" || error "ImagePullBackOff pods: $image_pull"
# shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
[[ -z "$crash_loop" ]] && ok "no CrashLoopBackOff issues" || error "CrashLoopBackOff pods: $crash_loop"

# Check for frequent restarts (threshold: >3 restarts)
high_restart_containers=$(kubectl get pods -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{range .status.containerStatuses[*]}{.name}{" "}{.restartCount}{"\n"}{end}{end}' 2>/dev/null | awk '$3 > 3 {print $1"/"$2 "(" $3 " restarts)"}' | tr '\n' ' ' | awk '{sub(/[ \t]+$/, "")} 1' || echo "")
# shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
[[ -z "$high_restart_containers" ]] && ok "no containers with excessive restarts" || error "high restart count: $high_restart_containers"

# Certificates (if cert-manager available)
if kubectl api-resources | grep -q certificates.cert-manager.io; then
    echo "=== Certificates ==="
    if [[ "$DATASTORE" == "postgresql" ]]; then
        certs=("postgresql-root-ca" "postgresql-server-cert" "postgresql-client-cert")
    else
        certs=("mongo-root-ca" "mongo-app-client-cert")
    fi

    for cert in "${certs[@]}"; do
        if kubectl get certificate "$cert" -n "$NAMESPACE" >/dev/null 2>&1; then
            ready=$(kubectl get certificate "$cert" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
            # shellcheck disable=SC2015  # && || pattern is intentional for conditional execution
            [[ "$ready" == "True" ]] && ok "$cert ready" || error "$cert not ready"
        else
            warn "$cert not found"
        fi
    done
fi

# Summary
echo "=== Summary ==="
if [[ "$ERRORS" -eq 0 ]]; then
    ok "NVSentinel deployment validation PASSED"
else
    error "NVSentinel deployment validation FAILED ($ERRORS errors)"
    exit 1
fi