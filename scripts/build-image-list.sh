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
#

# Exit on error, undefined variables, and pipe failures
set -euo pipefail

echo "Starting combined build_versions.sh"

# ------- Configuration -------
# Input file containing image repository and tag information
input_file="distros/kubernetes/nvsentinel/values.yaml"
# Base prefix for static images
static_prefix=""

# Set default values for container registry and organization
CONTAINER_REGISTRY=${CONTAINER_REGISTRY:-ghcr.io}
CONTAINER_ORG=${CONTAINER_ORG:-$(git config --get remote.origin.url | sed -n 's#.*/\([^/]*\)/[^/]*$#\1#p' | tr '[:upper:]' '[:lower:]')}
# Use provided SAFE_REF_NAME or calculate from CI_COMMIT_REF_NAME or git branch
if [[ -z "${SAFE_REF_NAME:-}" ]]; then
  CI_COMMIT_REF_NAME=${CI_COMMIT_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}
  # Sanitize branch name by replacing slashes with dashes
  SAFE_REF_NAME=${CI_COMMIT_REF_NAME//\//-}
fi

# Initialize output file
out="versions.txt"
: > "$out"

# ------- 1) Build dynamic list -------
# Define array of dynamic images with their respective tags (sorted!)
declare -a dynamic_images=(
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/csp-health-monitor:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/fault-quarantine:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/fault-remediation:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/file-server-cleanup:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/gpu-health-monitor:${SAFE_REF_NAME}-dcgm-3.x"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/gpu-health-monitor:${SAFE_REF_NAME}-dcgm-4.x"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/health-events-analyzer:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/janitor:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/labeler:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/log-collector:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/metadata-collector:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/node-drainer:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/platform-connectors:${SAFE_REF_NAME}"
  "${CONTAINER_REGISTRY}/${CONTAINER_ORG}/nvsentinel/syslog-health-monitor:${SAFE_REF_NAME}"
)

# Write dynamic images to output file
echo "Emitting dynamic images…"
for img in "${dynamic_images[@]}"; do
  echo "$img" >> "$out"
done

# Create associative array to track seen image names (without registry/org)
declare -A seen
for img in "${dynamic_images[@]}"; do
  # Extract just the image name (e.g., platform-connectors)
  repo="${img%%:*}"  # Remove tag
  image_name="${repo##*/}"  # Remove registry/org prefix
  seen["$image_name"]=1
done

# Create temporary file for static images
static_tmp=$(mktemp)

# Process input file to extract static images
# This awk script:
# 1. Extracts repository and tag information from values.yaml
# 2. Cleans up whitespace and quotes
# 3. Formats the output using the configured static prefix
awk -v prefix="$static_prefix" '
  function clean(v) {
    gsub(/^[ \t]*"/, "", v)
    gsub(/"[ \t]*$/, "", v)
    gsub(/^[ \t]+|[ \t]+$/, "", v)
    return v
  }
  /^[[:space:]]*repository:[[:space:]]*/ {
    repo = clean($2)
    if (!(repo in seen)) {
      order[++n] = repo
      tags[repo] = ""
    }
    next
  }
  /^[[:space:]]*tag:[[:space:]]*/ {
    t = clean($2)
    if (repo != "") {
      tags[repo] = t
      repo = ""
    }
    next
  }
  END {
    for (i = 1; i <= n; i++) {
      r = order[i]; suf = r; sub(".*/","",suf)
      t = (tags[r] == "" ? "main" : tags[r])
      print r ":" t
    }
  }
' "$input_file" > "$static_tmp"

# Merge static items that are not in the dynamic list
echo "Merging static images…"
while IFS= read -r img; do
  [[ -z "$img" ]] && continue
  # Extract just the image name for comparison
  repo="${img%%:*}"  # Remove tag
  image_name="${repo##*/}"  # Remove registry/org prefix
  if [[ -z "${seen[$image_name]+_}" ]]; then
    echo "$img" >> "$out"
  fi
done < "$static_tmp"

# Clean up temporary file
rm "$static_tmp"

# Display the generated output
echo
echo "Generated $out:"
cat "$out"
