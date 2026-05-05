#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

# Injects a minimal stub gpu_metadata.json onto each GPU worker node so the
# NIC health monitor DaemonSet can start. The E2E test overwrites this with
# real test metadata before exercising any scenarios.
#
# This script is only used in the Tilt dev environment — production nodes
# get their metadata from the metadata-collector DaemonSet.
set -euo pipefail

NAMESPACE="nvsentinel"

STUB_B64=$(printf '%s' '{"version":"1.0","timestamp":"2026-01-01T00:00:00Z","node_name":"stub","driver_version":"570.148.08","chassis_serial":null,"gpus":[{"gpu_id":0,"uuid":"GPU-stub-0","pci_address":"0001:00:00.0","serial_number":"0","device_name":"NVIDIA A100","nvlinks":[],"numa_node":0}],"nvswitches":[],"nic_topology":{"mlx5_0":["NODE"]}}' | base64 | tr -d '\n')

if ! nodes=$(kubectl get nodes -l nvsentinel.dgxc.nvidia.com/driver.installed=true \
    --no-headers -o custom-columns=NAME:.metadata.name 2>&1); then
    echo "ERROR: kubectl failed: $nodes" >&2
    exit 1
fi

if [ -z "$nodes" ]; then
    echo "No GPU worker nodes found, skipping metadata stub injection"
    exit 0
fi

failed=0

for node in $nodes; do
    # Skip KWOK nodes — they simulate pods but have no real filesystem.
    if kubectl get node "$node" -o jsonpath='{.metadata.annotations.kwok\.x-k8s\.io/node}' 2>/dev/null | grep -q 'fake'; then
        echo "Skipping KWOK node $node"
        continue
    fi

    pod_name="nic-metadata-stub-${node}"

    kubectl delete pod "$pod_name" -n "$NAMESPACE" --ignore-not-found --wait=true 2>/dev/null || true

    echo "Injecting stub metadata onto $node"

    tmpfile=$(mktemp)
    cat > "$tmpfile" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod_name}
  namespace: ${NAMESPACE}
spec:
  restartPolicy: Never
  nodeName: ${node}
  containers:
    - name: inject
      image: busybox:latest
      command: ["sh", "-c", "mkdir -p /host && [ -f /host/gpu_metadata.json ] || echo \$STUB | base64 -d > /host/gpu_metadata.json"]
      env:
        - name: STUB
          value: "${STUB_B64}"
      volumeMounts:
        - name: host
          mountPath: /host
  volumes:
    - name: host
      hostPath:
        path: /var/lib/nvsentinel
        type: DirectoryOrCreate
EOF

    kubectl apply -f "$tmpfile"
    rm -f "$tmpfile"

    echo "Waiting for inject pod to complete on $node"
    if ! kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
        pod/"$pod_name" -n "$NAMESPACE" --timeout=60s 2>/dev/null; then
        echo "ERROR: inject pod failed on $node" >&2
        kubectl logs "$pod_name" -n "$NAMESPACE" 2>/dev/null || true
        failed=1
    fi

    kubectl delete pod "$pod_name" -n "$NAMESPACE" --ignore-not-found --wait=false 2>/dev/null || true
done

if [ "$failed" -ne 0 ]; then
    echo "ERROR: metadata stub injection failed on one or more nodes" >&2
    exit 1
fi

echo "Stub metadata injection complete"
