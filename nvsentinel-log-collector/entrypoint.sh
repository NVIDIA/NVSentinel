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

mkdir -p "${ARTIFACTS_DIR}"
echo "[INFO] Target node: ${NODE_NAME} | GPU Operator namespace: ${GPU_OPERATOR_NAMESPACE} | Driver container: ${DRIVER_CONTAINER_NAME}"

# Locate the driver daemonset pod on the node
DRIVER_POD_NAME="$(kubectl -n "${GPU_OPERATOR_NAMESPACE}" get pods -l app=nvidia-driver-daemonset --field-selector spec.nodeName="${NODE_NAME}" -o name | head -n1 | cut -d/ -f2 || true)"

if [ -z "${DRIVER_POD_NAME}" ]; then
  echo "[ERROR] nvidia-driver-daemonset pod not found on node ${NODE_NAME} in namespace ${GPU_OPERATOR_NAMESPACE}" >&2
  exit 1
fi

# 1) Collect bug report from driver container
BUG_REPORT_REMOTE_BASE="/var/tmp/nvidia-bug-report-${NODE_NAME}-${TIMESTAMP}"
BUG_REPORT_LOCAL_BASE="${ARTIFACTS_DIR}/nvidia-bug-report-${NODE_NAME}-${TIMESTAMP}"
BUG_REPORT_REMOTE_PATH="${BUG_REPORT_REMOTE_BASE}.log.gz"

kubectl -n "${GPU_OPERATOR_NAMESPACE}" exec -c "${DRIVER_CONTAINER_NAME}" "${DRIVER_POD_NAME}" -- \
  nvidia-bug-report.sh --output-file "${BUG_REPORT_REMOTE_BASE}.log"

# Copy the bug report (assume fixed path), with a simple retry
BUG_REPORT_LOCAL="${BUG_REPORT_LOCAL_BASE}.log.gz"
if ! kubectl -n "${GPU_OPERATOR_NAMESPACE}" cp "${DRIVER_POD_NAME}:${BUG_REPORT_REMOTE_PATH}" "${BUG_REPORT_LOCAL}"; then
  sleep 2
  kubectl -n "${GPU_OPERATOR_NAMESPACE}" cp "${DRIVER_POD_NAME}:${BUG_REPORT_REMOTE_PATH}" "${BUG_REPORT_LOCAL}"
fi
echo "[INFO] Bug report saved to ${BUG_REPORT_LOCAL}"

# 2) GPU Operator must-gather
GPU_MG_DIR="${ARTIFACTS_DIR}/gpu-operator-must-gather"
mkdir -p "${GPU_MG_DIR}"
curl -fsSL "${MUST_GATHER_SCRIPT_URL}" -o "${GPU_MG_DIR}/must-gather.sh"
chmod +x "${GPU_MG_DIR}/must-gather.sh"
bash "${GPU_MG_DIR}/must-gather.sh"
GPU_MG_TARBALL="${ARTIFACTS_DIR}/gpu-operator-must-gather-${NODE_NAME}-${TIMESTAMP}.tar.gz"
tar -C "${GPU_MG_DIR}" -czf "${GPU_MG_TARBALL}" .

# Optional upload to in-cluster file server
if [ -n "${UPLOAD_URL_BASE:-}" ]; then
  echo "[INFO] Uploading artifacts to ${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}"
  if [ -f "${BUG_REPORT_LOCAL}" ]; then
    curl -fsS -X PUT --upload-file "${BUG_REPORT_LOCAL}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${BUG_REPORT_LOCAL}")" || true
  fi
  if [ -f "${GPU_MG_TARBALL}" ]; then
    curl -fsS -X PUT --upload-file "${GPU_MG_TARBALL}" \
      "${UPLOAD_URL_BASE}/${NODE_NAME}/${TIMESTAMP}/$(basename "${GPU_MG_TARBALL}")" || true
  fi
fi

echo "[INFO] Done. Artifacts under ${ARTIFACTS_DIR}"
