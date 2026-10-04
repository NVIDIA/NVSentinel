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

# Shared settings and helpers for the local fault injection demo scripts.
# Source this, do not execute it.

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-nvsentinel-demo}"
NAMESPACE="${NAMESPACE:-nvsentinel}"
DCGM_NAMESPACE="${DCGM_NAMESPACE:-gpu-operator}"
WORKLOAD_NAMESPACE="${WORKLOAD_NAMESPACE:-default}"
WORKLOAD_NAME="${WORKLOAD_NAME:-gpu-workload}"

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck disable=SC2034  # read by the scripts that source this file
REPO_ROOT="$(cd "$DEMO_DIR/../.." && pwd)"
# shellcheck disable=SC2034  # read by the scripts that source this file
CONFIG_DIR="$DEMO_DIR/config"

if [[ -t 1 ]]; then
    RED=$'\033[0;31m'
    GREEN=$'\033[0;32m'
    YELLOW=$'\033[1;33m'
    BLUE=$'\033[0;34m'
    BOLD=$'\033[1m'
    NC=$'\033[0m'
else
    RED='' GREEN='' YELLOW='' BLUE='' BOLD='' NC=''
fi

log() { echo "${BLUE}[INFO]${NC} $*"; }
success() { echo "${GREEN}[ OK ]${NC} $*"; }
warn() { echo "${YELLOW}[WARN]${NC} $*" >&2; }

# Prints the message and exits non-zero. Every caller treats a failure here as
# fatal, so this never returns.
fail() {
    echo "${RED}[FAIL]${NC} $*" >&2
    exit 1
}

section() {
    echo
    echo "${BOLD}=============================================================${NC}"
    echo "${BOLD}  $*${NC}"
    echo "${BOLD}=============================================================${NC}"
    echo
}

# Points at the next step to run by hand. demo.sh sets DEMO_ONESHOT because it
# is about to run that step itself, and printing the suggestion between every
# stage of an unattended run is just noise.
next_step() {
    [[ -z "${DEMO_ONESHOT:-}" ]] || return 0

    section "Next"
    printf '  %s\n' "$@"
}

require_tools() {
    local missing=()
    local tool
    for tool in "$@"; do
        command -v "$tool" &>/dev/null || missing+=("$tool")
    done

    if [[ ${#missing[@]} -gt 0 ]]; then
        fail "Missing required tools: ${missing[*]}. See README.md for install links."
    fi
}

require_cluster() {
    if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
        fail "Cluster '$CLUSTER_NAME' not found. Run ./scripts/00-setup.sh first."
    fi
    kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null
}

# Echoes the name of the node the demo treats as the GPU node: the first node
# carrying the GPU label that 00-setup.sh applies.
gpu_node() {
    local node
    node="$(kubectl get nodes -l nvidia.com/gpu.present=true \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"

    [[ -n "$node" ]] || fail "No node labelled nvidia.com/gpu.present=true. Run ./scripts/00-setup.sh first."
    echo "$node"
}

is_cordoned() {
    local node=${1:?is_cordoned requires a node name}
    [[ "$(kubectl get node "$node" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)" == "true" ]]
}

# Echoes the status of a node condition, or nothing if the condition is absent.
node_condition_status() {
    local node=${1:?node_condition_status requires a node name}
    local condition=${2:?node_condition_status requires a condition type}

    kubectl get node "$node" -o json 2>/dev/null |
        jq -r --arg type "$condition" \
            '.status.conditions[]? | select(.type == $type) | .status'
}

# Waits for a pod matching the selector to exist, then for it to be Ready.
#
# The existence poll is not redundant: `kubectl wait` resolves the selector once,
# up front, and exits immediately with "no matching resources found" if nothing
# matches yet. Its --timeout never covers the gap between creating a DaemonSet
# and its controller creating the pod, so waiting on readiness alone is a race.
#   wait_for_pod_ready <namespace> <selector> <timeout-seconds>
wait_for_pod_ready() {
    local namespace=${1:?wait_for_pod_ready requires a namespace}
    local selector=${2:?wait_for_pod_ready requires a selector}
    local timeout=${3:?wait_for_pod_ready requires a timeout}
    local deadline=$((SECONDS + timeout))

    while ((SECONDS < deadline)); do
        if [[ -n "$(kubectl get pods -n "$namespace" -l "$selector" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)" ]]; then
            kubectl wait --for=condition=ready pod -l "$selector" \
                -n "$namespace" --timeout="$((deadline - SECONDS))s" >/dev/null 2>&1
            return
        fi
        sleep 2
    done

    return 1
}

# Resolves the NVSentinel Helm chart version to install.
#
# NVSENTINEL_CHART_VERSION pins it. Otherwise the highest semver tag published
# to the chart's OCI repository wins, so a clone of this repo installs the
# current release no matter how old the clone is. Falls back to the latest
# GitHub release when the registry cannot be listed.
resolve_chart_version() {
    if [[ -n "${NVSENTINEL_CHART_VERSION:-}" ]]; then
        echo "$NVSENTINEL_CHART_VERSION"
        return
    fi

    local token version
    token="$(curl -fsSL --max-time 30 \
        "https://ghcr.io/token?scope=repository:nvidia/nvsentinel:pull&service=ghcr.io" \
        2>/dev/null | jq -r '.token // empty')"

    if [[ -n "$token" ]]; then
        version="$(curl -fsSL --max-time 30 -H "Authorization: Bearer $token" \
            "https://ghcr.io/v2/nvidia/nvsentinel/tags/list" 2>/dev/null |
            jq -r '.tags[]? | select(test("^v[0-9]+\\.[0-9]+\\.[0-9]+$"))' |
            sed 's/^v//' | sort -V | tail -1)"

        if [[ -n "$version" ]]; then
            echo "v${version}"
            return
        fi
    fi

    version="$(curl -fsSL --max-time 30 \
        "https://api.github.com/repos/NVIDIA/NVSentinel/releases/latest" \
        2>/dev/null | jq -r '.tag_name // empty')"

    if [[ -n "$version" ]]; then
        echo "$version"
        return
    fi

    fail "Could not determine the latest NVSentinel version from ghcr.io or github.com.
Check your network, or pin one explicitly:
  NVSENTINEL_CHART_VERSION=v1.23.0 ./scripts/00-setup.sh"
}

# Polls until the command succeeds or the timeout expires. Prints a dot per
# attempt so a long wait does not look like a hang.
#   wait_until <timeout-seconds> <description> <command...>
#
# On timeout it prints whatever the last attempt wrote to stderr. Output is
# suppressed while polling, so without this a condition that never becomes true
# and a check that was broken all along look identical.
wait_until() {
    local timeout=$1 description=$2
    shift 2
    local deadline=$((SECONDS + timeout))
    local stderr

    printf '%s' "       waiting for ${description} "
    while ((SECONDS < deadline)); do
        if stderr="$("$@" 2>&1 >/dev/null)"; then
            printf ' done (%ss)\n' "$((timeout - (deadline - SECONDS)))"
            return 0
        fi
        printf '.'
        sleep 3
    done

    printf ' timed out after %ss\n' "$timeout"
    [[ -n "$stderr" ]] && warn "last attempt reported: $stderr"
    return 1
}
