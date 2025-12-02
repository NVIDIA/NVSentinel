#!/bin/bash
# Mock GPU Operator must-gather for testing
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

set -e

echo "[MOCK] GPU Operator must-gather called"
echo "[MOCK] Collecting mock diagnostic data..."

# Create mock must-gather directory structure
mkdir -p ./namespaces ./logs ./cluster-info

# Generate simple mock files
cat > ./cluster-info/info.txt <<EOF
Mock GPU Operator Must-Gather
Generated: $(date)
Node: ${NODE_NAME:-unknown}
Mock Mode: Enabled
EOF

echo "Mock pod logs - $(date)" > ./logs/mock-pod.log
echo "Mock namespace data" > ./namespaces/mock-ns.yaml

echo "[MOCK] Must-gather complete"
exit 0
