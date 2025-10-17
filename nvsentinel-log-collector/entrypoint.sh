#!/usr/bin/env bash
#
# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
set -xeuo pipefail

# Simple collector:
# - Run nvidia-bug-report inside the node's nvidia-driver-daemonset pod
# - Run GPU Operator must-gather
# - Optionally upload both artifacts to an in-cluster file server if UPLOAD_URL_BASE is set

NODE_NAME="${NODE_NAME:-unknown-node}"
TIMESTAMP="${TIMESTAMP:-$(date +%Y%m%d-%H%M%S)}"
ARTIFACTS_BASE="${ARTIFACTS_BASE:-/artifacts}"
ARTIFACTS_DIR="${ARTIFACTS_BASE}/${NODE_NAME}/${TIMESTAMP}"
GPU_OPERATOR_NAMESPACE="${GPU_OPERATOR_NAMESPACE:-gpu-operator}"
DRIVER_CONTAINER_NAME="${DRIVER_CONTAINER_NAME:-nvidia-driver-ctr}"
MUST_GATHER_SCRIPT_URL="${MUST_GATHER_SCRIPT_URL:-https://raw.githubusercontent.com/NVIDIA/gpu-operator/main/hack/must-gather.sh}"
ENABLE_GCP_SOS_COLLECTION="${ENABLE_GCP_SOS_COLLECTION:-false}"

mkdir -p "${ARTIFACTS_DIR}"
echo "[INFO] Target node: ${NODE_NAME} | GPU Operator namespace: ${GPU_OPERATOR_NAMESPACE} | Driver container: ${DRIVER_CONTAINER_NAME}"

# Function to detect if running on GCP using IMDS 
is_running_on_gcp() {
  local timeout=5
  if curl -s -m "${timeout}" -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/" >/dev/null 2>&1; then
    return 0
  else
    return 1
  fi
}

# Auto-detect nvidia-bug-report approach and collect GCP SOS if needed
GCP_SOS_REPORT=""
# Access host filesystem directly through privileged container
GCP_NVIDIA_BUG_REPORT="/host/home/kubernetes/bin/nvidia/bin/nvidia-bug-report.sh"

# 1) Collect nvidia-bug-report - auto-detect approach
# Check if GCP COS nvidia-bug-report exists on the host filesystem (accessed via privileged container)
if [ -f "${GCP_NVIDIA_BUG_REPORT}" ]; then
  echo "[INFO] Found nvidia-bug-report at GCP COS location: ${GCP_NVIDIA_BUG_REPORT}"
  
  # Use GCP COS approach - write directly to container filesystem
  BUG_REPORT_LOCAL_BASE="${ARTIFACTS_DIR}/nvidia-bug-report-${NODE_NAME}-${TIMESTAMP}"
  BUG_REPORT_LOCAL="${BUG_REPORT_LOCAL_BASE}.log.gz"
  
  # Run nvidia-bug-report and output directly to artifacts directory
  "${GCP_NVIDIA_BUG_REPORT}" --output-file "${BUG_REPORT_LOCAL_BASE}.log"
  echo "[INFO] Bug report saved to ${BUG_REPORT_LOCAL}"

else
  echo "[INFO] GCP COS nvidia-bug-report not found, using standard GPU Operator approach"
  
  # Locate the driver daemonset pod on the node
  DRIVER_POD_NAME="$(kubectl -n "${GPU_OPERATOR_NAMESPACE}" get pods -l app=nvidia-driver-daemonset --field-selector spec.nodeName="${NODE_NAME}" -o name | head -n1 | cut -d/ -f2 || true)"

  if [ -z "${DRIVER_POD_NAME}" ]; then
    echo "[ERROR] nvidia-driver-daemonset pod not found on node ${NODE_NAME} in namespace ${GPU_OPERATOR_NAMESPACE}" >&2
    echo "[ERROR] This might be a GCP cluster with preinstalled drivers but nvidia-bug-report not accessible" >&2
    echo "[ERROR] Failed to collect nvidia-bug-report using available methods" >&2
    exit 1
  fi

  # Collect bug report from driver container
  BUG_REPORT_REMOTE_BASE="/var/tmp/nvidia-bug-report-${NODE_NAME}-${TIMESTAMP}"
  BUG_REPORT_LOCAL_BASE="${ARTIFACTS_DIR}/nvidia-bug-report-${NODE_NAME}-${TIMESTAMP}"
  BUG_REPORT_REMOTE_PATH="${BUG_REPORT_REMOTE_BASE}.log.gz"

  kubectl -n "${GPU_OPERATOR_NAMESPACE}" exec -c "${DRIVER_CONTAINER_NAME}" "${DRIVER_POD_NAME}" -- \
    nvidia-bug-report.sh --output-file "${BUG_REPORT_REMOTE_BASE}.log"

  # Copy the bug report with retry
  BUG_REPORT_LOCAL="${BUG_REPORT_LOCAL_BASE}.log.gz"
  if ! kubectl -n "${GPU_OPERATOR_NAMESPACE}" cp "${DRIVER_POD_NAME}:${BUG_REPORT_REMOTE_PATH}" "${BUG_REPORT_LOCAL}"; then
    sleep 2
    kubectl -n "${GPU_OPERATOR_NAMESPACE}" cp "${DRIVER_POD_NAME}:${BUG_REPORT_REMOTE_PATH}" "${BUG_REPORT_LOCAL}"
  fi
  echo "[INFO] Bug report saved to ${BUG_REPORT_LOCAL}"
fi

# 2) Collect GCP SOS report if on GCP and enabled
if is_running_on_gcp && [ "${ENABLE_GCP_SOS_COLLECTION}" = "true" ]; then
  echo "[INFO] Detected GCP environment and SOS collection is enabled, collecting SOS report..."
  
  # Try both sosreport and sos report commands based on GCP documentation
  # https://cloud.google.com/container-optimized-os/docs/how-to/sosreport#cos-85-and-earlier
  SOS_SUCCESS=false
  
  if command -v sosreport >/dev/null 2>&1; then
    echo "[INFO] Running sosreport (COS 85 and earlier)..."
    # Write directly to artifacts directory instead of host /var
    sudo sosreport --all-logs --batch --tmp-dir="${ARTIFACTS_DIR}" >/dev/null 2>&1 && SOS_SUCCESS=true || true
  elif command -v sos >/dev/null 2>&1; then
    echo "[INFO] Running sos report (COS 105 and later)..."
    # Write directly to artifacts directory instead of host /var
    sudo sos report --all-logs --batch --tmp-dir="${ARTIFACTS_DIR}" >/dev/null 2>&1 && SOS_SUCCESS=true || true
  else
    echo "[WARNING] Neither sosreport nor sos command found. SOS report collection will be skipped." >&2
    echo "[WARNING] To enable SOS report collection, ensure sosreport package is installed on the node." >&2
    echo "[INFO] Continuing without SOS report collection..."
  fi
  
  # Find the generated SOS report if successful
  if [ "$SOS_SUCCESS" = true ]; then
    SOS_REPORT_PATH=$(find "${ARTIFACTS_DIR}" -name "sosreport-*.tar.*" -mmin -10 | head -1)
    
    if [ -n "${SOS_REPORT_PATH}" ] && [ -f "${SOS_REPORT_PATH}" ]; then
      GCP_SOS_REPORT="${SOS_REPORT_PATH}"  # Already in artifacts directory
      echo "[INFO] GCP SOS report saved to ${GCP_SOS_REPORT}"
    else
      echo "[WARN] Failed to locate generated SOS report"
    fi
  fi
elif [ "${ENABLE_GCP_SOS_COLLECTION}" = "true" ]; then
  echo "[INFO] SOS collection is enabled but not running on GCP, skipping SOS report collection"
else
  echo "[INFO] SOS collection is disabled or not applicable for this environment"
fi

# 2) GPU Operator must-gather (for all clusters)
GPU_MG_DIR="${ARTIFACTS_DIR}/gpu-operator-must-gather"
mkdir -p "${GPU_MG_DIR}"
echo "[INFO] Running GPU Operator must-gather..."
curl -fsSL "${MUST_GATHER_SCRIPT_URL}" -o "${GPU_MG_DIR}/must-gather.sh"
chmod +x "${GPU_MG_DIR}/must-gather.sh"
bash "${GPU_MG_DIR}/must-gather.sh"

GPU_MG_TARBALL="${ARTIFACTS_DIR}/gpu-operator-must-gather-${NODE_NAME}-${TIMESTAMP}.tar.gz"
if [ -d "${GPU_MG_DIR}" ] && [ "$(ls -A "${GPU_MG_DIR}" 2>/dev/null)" ]; then
  tar -C "${GPU_MG_DIR}" -czf "${GPU_MG_TARBALL}" .
  echo "[INFO] GPU Operator must-gather tarball created: ${GPU_MG_TARBALL}"
else
  echo "[INFO] No GPU Operator must-gather data to archive"
fi

# Optional upload to in-cluster file server
if [ -n "${UPLOAD_URL_BASE:-}" ]; then
  echo "[INFO] Uploading artifacts to ${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}"
  if [ -f "${BUG_REPORT_LOCAL}" ]; then
    curl -fsS -X PUT --upload-file "${BUG_REPORT_LOCAL}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${BUG_REPORT_LOCAL}")" || true
    echo "[UPLOAD_SUCCESS] nvidia-bug-report uploaded: $(basename "${BUG_REPORT_LOCAL}")"
  fi
  if [ -f "${GPU_MG_TARBALL}" ]; then
    curl -fsS -X PUT --upload-file "${GPU_MG_TARBALL}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${GPU_MG_TARBALL}")" || true
    echo "[UPLOAD_SUCCESS] gpu-operator must-gather uploaded: $(basename "${GPU_MG_TARBALL}")"
  fi
  if [ -n "${GCP_SOS_REPORT}" ] && [ -f "${GCP_SOS_REPORT}" ]; then
    curl -fsS -X PUT --upload-file "${GCP_SOS_REPORT}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${GCP_SOS_REPORT}")" || true
    echo "[UPLOAD_SUCCESS] GCP SOS report uploaded: $(basename "${GCP_SOS_REPORT}")"
  fi
fi

echo "[INFO] Done. Artifacts under ${ARTIFACTS_DIR}"