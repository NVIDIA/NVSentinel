#!/usr/bin/env bash
#
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
ENABLE_AWS_SOS_COLLECTION="${ENABLE_AWS_SOS_COLLECTION:-false}"

# Mock mode for testing - prepends mock scripts to PATH
MOCK_MODE="${MOCK_MODE:-false}"

if [ "${MOCK_MODE}" = "true" ]; then
  echo "[MOCK] Enabling mock mode - using mock nvidia-bug-report.sh and must-gather.sh"
  MOCK_SCRIPTS_DIR="/opt/log-collector"

  cp "${MOCK_SCRIPTS_DIR}/mock-nvidia-bug-report.sh" "${MOCK_SCRIPTS_DIR}/nvidia-bug-report.sh"
  cp "${MOCK_SCRIPTS_DIR}/mock-must-gather.sh" "${MOCK_SCRIPTS_DIR}/must-gather.sh"
  chmod +x "${MOCK_SCRIPTS_DIR}/nvidia-bug-report.sh" "${MOCK_SCRIPTS_DIR}/must-gather.sh"

  # Prepend to PATH so mocks are used instead of real tools
  export PATH="${MOCK_SCRIPTS_DIR}:${PATH}"

  # Override to use local mock instead of downloading
  MUST_GATHER_SCRIPT_URL="file://${MOCK_SCRIPTS_DIR}/must-gather.sh"

  echo "[MOCK] Mock mode enabled. nvidia-bug-report.sh and must-gather.sh will use mock versions."
fi

mkdir -p "${ARTIFACTS_DIR}"
echo "[INFO] Target node: ${NODE_NAME} | GPU Operator namespace: ${GPU_OPERATOR_NAMESPACE} | Driver container: ${DRIVER_CONTAINER_NAME}"

is_running_on_gcp() {
  local timeout=5
  if curl -s -m "${timeout}" -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/" >/dev/null 2>&1; then
    return 0
  else
    return 1
  fi
}

# Function to detect if running on AWS using IMDSv2 only
is_running_on_aws() {
  local timeout=5
  local response
  local http_code
  local token
  
  response=$(curl -s -w "\n%{http_code}" -m "${timeout}" -X PUT \
    "http://169.254.169.254/latest/api/token" \
    -H "X-aws-ec2-metadata-token-ttl-seconds: 21600" 2>/dev/null)
  
  http_code=$(echo "$response" | tail -n1)
  echo "[DEBUG] AWS IMDSv2 token endpoint - HTTP $http_code, Response: ${response}" >&2
  if [ "$http_code" != "200" ]; then
    return 1
  fi
  
  token=$(echo "$response" | head -n-1)
  if curl -s -m "${timeout}" -H "X-aws-ec2-metadata-token: ${token}" \
    "http://169.254.169.254/latest/meta-data/" >/dev/null 2>&1; then
    return 0
  else
    return 1
  fi
}

# Auto-detect nvidia-bug-report approach and collect SOS reports if needed
GCP_SOS_REPORT=""
AWS_SOS_REPORT=""
# Access host filesystem directly through privileged container
GCP_NVIDIA_BUG_REPORT="/host/home/kubernetes/bin/nvidia/bin/nvidia-bug-report.sh"

# 1) Collect nvidia-bug-report - auto-detect approach
# In mock mode, use the local mock script directly
if [ "${MOCK_MODE}" = "true" ]; then
  echo "[MOCK] Using local mock nvidia-bug-report.sh"
  BUG_REPORT_LOCAL_BASE="${ARTIFACTS_DIR}/nvidia-bug-report-${NODE_NAME}-${TIMESTAMP}"
  BUG_REPORT_LOCAL="${BUG_REPORT_LOCAL_BASE}.log.gz"
  nvidia-bug-report.sh --output-file "${BUG_REPORT_LOCAL_BASE}.log"
  echo "[MOCK] Bug report saved to ${BUG_REPORT_LOCAL}"

# Check if GCP COS nvidia-bug-report exists on the host filesystem (accessed via privileged container)
elif [ -f "${GCP_NVIDIA_BUG_REPORT}" ]; then
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
  
  # Run SOS report on the host node via chroot (based on Google Cloud documentation)
  # https://cloud.google.com/container-optimized-os/docs/how-to/sosreport#cos-85-and-earlier
  SOS_SUCCESS=false
  SOS_OUTPUT=""
  
  # Check which sos command is available on the host (COS 105+ uses 'sos report', COS 85 and earlier uses 'sosreport')
  if chroot /host bash -c "command -v sos" >/dev/null 2>&1; then
    echo "[INFO] Running sos report (COS 105 and later)..."
    # Run sos report on the host filesystem and capture output
    SOS_OUTPUT=$(chroot /host bash -c "sos report --all-logs --batch --tmp-dir=/var/tmp" 2>&1) && SOS_SUCCESS=true || true
  elif chroot /host bash -c "command -v sosreport" >/dev/null 2>&1; then
    echo "[INFO] Running sosreport (COS 85 and earlier)..."
    # Run sosreport on the host filesystem and capture output
    SOS_OUTPUT=$(chroot /host bash -c "sosreport --all-logs --batch --tmp-dir=/var/tmp" 2>&1) && SOS_SUCCESS=true || true
  else
    echo "[WARNING] Neither sos nor sosreport command found on the host node." >&2
    echo "[WARNING] SOS report collection will be skipped." >&2
    echo "[INFO] Continuing without SOS report collection..."
  fi
  
  # Find and copy the generated SOS report if successful
  if [ "$SOS_SUCCESS" = true ]; then
    # SOS report is on host filesystem at /var/tmp
    # Filename pattern: sosreport-<hostname>-<timestamp>.tar.xz (generated by sos tool)
    # Find the most recently created SOS report (within last 5 minutes)
    SOS_REPORT_PATH=$(find /host/var/tmp -name "sosreport-*.tar.*" -mmin -5 -not -name "*.sha256" 2>/dev/null | sort -r | head -1)
    
    if [ -n "${SOS_REPORT_PATH}" ] && [ -f "${SOS_REPORT_PATH}" ]; then
      # Copy from host to artifacts directory
      SOS_REPORT_FILENAME=$(basename "${SOS_REPORT_PATH}")
      GCP_SOS_REPORT="${ARTIFACTS_DIR}/${SOS_REPORT_FILENAME}"
      cp "${SOS_REPORT_PATH}" "${GCP_SOS_REPORT}"
      echo "[INFO] GCP SOS report saved to ${GCP_SOS_REPORT}"
    else
      echo "[WARN] SOS report generated but file not found in /host/var/tmp" >&2
      echo "[DEBUG] SOS command output: ${SOS_OUTPUT}" >&2
    fi
  fi
elif [ "${ENABLE_GCP_SOS_COLLECTION}" = "true" ]; then
  echo "[INFO] SOS collection is enabled but not running on GCP, skipping SOS report collection"
else
  echo "[INFO] SOS collection is disabled or not applicable for this environment"
fi

# 3) Collect AWS SOS report if on AWS and enabled
if is_running_on_aws && [ "${ENABLE_AWS_SOS_COLLECTION}" = "true" ]; then
  echo "[INFO] Collecting AWS SOS report..."
  
  # Generate a unique identifier for this SOS report 
  SOS_UNIQUE_ID="nvsentinel-$(date +%s)-$$"
  
  if chroot /host bash -c "sos report --batch --tmp-dir=/var/tmp --name=${SOS_UNIQUE_ID}"; then
    # Find the SOS report with our unique identifier (exclude .sha256 checksum files)
    # Note: sos report prepends hostname, so pattern is: sosreport-<hostname>-<our-unique-id>-<date>-<random>.tar.*
    AWS_SOS_REPORT_PATH=$(find /host/var/tmp -name "sosreport-*-${SOS_UNIQUE_ID}-*.tar.*" -not -name "*.sha256" 2>/dev/null | head -1)
    
    if [ -n "${AWS_SOS_REPORT_PATH}" ] && [ -f "${AWS_SOS_REPORT_PATH}" ]; then
      AWS_SOS_REPORT="${ARTIFACTS_DIR}/$(basename "${AWS_SOS_REPORT_PATH}")"
      cp "${AWS_SOS_REPORT_PATH}" "${AWS_SOS_REPORT}" && echo "[INFO] AWS SOS report saved to ${AWS_SOS_REPORT}"
    else
      echo "[WARN] SOS report generated but file with unique ID ${SOS_UNIQUE_ID} not found"
    fi
  else
    echo "[WARN] SOS report collection failed - sos may not be installed on host"
  fi
  
elif [ "${ENABLE_AWS_SOS_COLLECTION}" = "true" ]; then
  echo "[INFO] AWS SOS collection enabled but not on AWS - skipping"
fi

# 4) GPU Operator must-gather (optional - disabled by default)
ENABLE_GPU_OPERATOR_MUST_GATHER="${ENABLE_GPU_OPERATOR_MUST_GATHER:-false}"
GPU_MG_TARBALL=""

if [ "${ENABLE_GPU_OPERATOR_MUST_GATHER}" = "true" ]; then
GPU_MG_DIR="${ARTIFACTS_DIR}/gpu-operator-must-gather"
mkdir -p "${GPU_MG_DIR}"
  echo "[INFO] Running GPU Operator must-gather (this may take a while for large clusters)..."
  
  MUST_GATHER_SUCCESS=true

  # Download must-gather script with error handling
  if curl -fsSL "${MUST_GATHER_SCRIPT_URL}" -o "${GPU_MG_DIR}/must-gather.sh"; then
chmod +x "${GPU_MG_DIR}/must-gather.sh"
    # Run must-gather with error handling to allow partial artifact collection
    # If must-gather fails (timeout, network issues, etc.), continue with available artifacts
    bash "${GPU_MG_DIR}/must-gather.sh" || {
      echo "[WARN] GPU Operator must-gather failed - continuing with available artifacts" >&2
      MUST_GATHER_SUCCESS=false
    }
  else
    echo "[WARN] Failed to download GPU Operator must-gather script - continuing without must-gather data" >&2
    MUST_GATHER_SUCCESS=false
  fi

GPU_MG_TARBALL="${ARTIFACTS_DIR}/gpu-operator-must-gather-${NODE_NAME}-${TIMESTAMP}.tar.gz"
  if [ "${MUST_GATHER_SUCCESS}" = "true" ] && [ -d "${GPU_MG_DIR}" ] && [ "$(ls -A "${GPU_MG_DIR}" 2>/dev/null)" ]; then
  tar -C "${GPU_MG_DIR}" -czf "${GPU_MG_TARBALL}" .
  echo "[INFO] GPU Operator must-gather tarball created: ${GPU_MG_TARBALL}"
else
  echo "[INFO] No GPU Operator must-gather data to archive"
    GPU_MG_TARBALL=""
  fi
else
  echo "[INFO] GPU Operator must-gather is disabled (ENABLE_GPU_OPERATOR_MUST_GATHER=${ENABLE_GPU_OPERATOR_MUST_GATHER})"
  echo "[INFO] To enable, set ENABLE_GPU_OPERATOR_MUST_GATHER=true and increase timeout for large clusters"
fi

# Optional upload to in-cluster file server
if [ -n "${UPLOAD_URL_BASE:-}" ]; then
  echo "[INFO] Uploading artifacts to ${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}"
  
  if [ -f "${BUG_REPORT_LOCAL}" ]; then
    if curl -fsS -X PUT --upload-file "${BUG_REPORT_LOCAL}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${BUG_REPORT_LOCAL}")"; then
      echo "[UPLOAD_SUCCESS] nvidia-bug-report uploaded: $(basename "${BUG_REPORT_LOCAL}")"
    else
      echo "[UPLOAD_FAILED] Failed to upload nvidia-bug-report: $(basename "${BUG_REPORT_LOCAL}")" >&2
    fi
  fi
  
  if [ -n "${GPU_MG_TARBALL}" ] && [ -f "${GPU_MG_TARBALL}" ]; then
    if curl -fsS -X PUT --upload-file "${GPU_MG_TARBALL}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${GPU_MG_TARBALL}")"; then
      echo "[UPLOAD_SUCCESS] gpu-operator must-gather uploaded: $(basename "${GPU_MG_TARBALL}")"
    else
      echo "[UPLOAD_FAILED] Failed to upload gpu-operator must-gather: $(basename "${GPU_MG_TARBALL}")" >&2
    fi
  fi
  
  if [ -n "${GCP_SOS_REPORT}" ] && [ -f "${GCP_SOS_REPORT}" ]; then
    if curl -fsS -X PUT --upload-file "${GCP_SOS_REPORT}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${GCP_SOS_REPORT}")"; then
      echo "[UPLOAD_SUCCESS] GCP SOS report uploaded: $(basename "${GCP_SOS_REPORT}")"
    else
      echo "[UPLOAD_FAILED] Failed to upload GCP SOS report: $(basename "${GCP_SOS_REPORT}")" >&2
    fi
  fi
  
  if [ -n "${AWS_SOS_REPORT}" ] && [ -f "${AWS_SOS_REPORT}" ]; then
    if curl -fsS -X PUT --upload-file "${AWS_SOS_REPORT}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${AWS_SOS_REPORT}")"; then
      echo "[UPLOAD_SUCCESS] AWS SOS report uploaded: $(basename "${AWS_SOS_REPORT}")"
    else
      echo "[UPLOAD_FAILED] Failed to upload AWS SOS report: $(basename "${AWS_SOS_REPORT}")" >&2
    fi
  fi
else
  echo "[INFO] No UPLOAD_URL_BASE configured - artifacts created locally at ${ARTIFACTS_DIR}"
fi

echo "[INFO] Done. Artifacts under ${ARTIFACTS_DIR}"