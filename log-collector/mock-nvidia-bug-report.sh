#!/bin/bash
# Mock nvidia-bug-report.sh for testing
# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

set -e

echo "[MOCK] nvidia-bug-report.sh called"

# Parse arguments to find output file
OUTPUT_FILE=""
while [[ $# -gt 0 ]]; do
  case $1 in
    --output-file)
      OUTPUT_FILE="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done

if [ -z "$OUTPUT_FILE" ]; then
  echo "[MOCK ERROR] No output file specified" >&2
  exit 1
fi

echo "[MOCK] Generating mock nvidia-bug-report to: ${OUTPUT_FILE}"

# Create a simple mock bug report and gzip it
# Real nvidia-bug-report.sh automatically appends .gz to the output file
# We mimic this behavior
echo "Mock NVIDIA Bug Report - Node: ${NODE_NAME:-unknown}, Date: $(date)" | gzip > "${OUTPUT_FILE}.gz"

echo "[MOCK] Mock nvidia-bug-report created successfully at ${OUTPUT_FILE}.gz"
exit 0