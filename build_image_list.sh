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
#

# Exit on error, undefined variables, and pipe failures
set -euo pipefail

echo "Starting combined build_versions.sh"

# ------- Configuration -------
# Input file containing image repository and tag information
input_file="distros/kubernetes/nvsentinel/values.yaml"
# Base prefix for static images
static_prefix="nvcr.io/nv-ngc-devops"

# Set default values for container registry and organization
NVCR_CONTAINER_REPO=${NVCR_CONTAINER_REPO:-nvcr.io}
NGC_ORG=${NGC_ORG:-nv-ngc-devops}
# Get current branch name or use git branch name as fallback
CI_COMMIT_REF_NAME=${CI_COMMIT_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}
# Sanitize branch name by replacing slashes with dashes
SAFE_REF_NAME=$(echo "$CI_COMMIT_REF_NAME" | sed 's#/#-#g')

# Initialize output file
out="versions.txt"
> "$out"

# ------- 1) Build dynamic list -------
# Define array of dynamic images with their respective tags
declare -a dynamic_images=(
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-gpu-health-monitor:${SAFE_REF_NAME}-dcgm-4.x"
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-nic-health-monitor:${SAFE_REF_NAME}"
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-nvswitch-health-monitor:${SAFE_REF_NAME}"
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-platform-connectors:${SAFE_REF_NAME}"
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-fault-quarantine-module:${SAFE_REF_NAME}"
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-node-health-events-uds-connector-server:${SAFE_REF_NAME}"
  "${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-node-drainer-module:${SAFE_REF_NAME}"
)

# Write dynamic images to output file
echo "Emitting dynamic images…"
for img in "${dynamic_images[@]}"; do
  echo "$img" >> "$out"
done

# Create associative array to track seen repositories
declare -A seen
for img in "${dynamic_images[@]}"; do
  seen["${img%%:*}"]=1
done

# Create temporary file for static images
static_tmp=$(mktemp)

# Process input file to extract static images
# This awk script:
# 1. Extracts repository and tag information from values.yaml
# 2. Cleans up whitespace and quotes
# 3. Formats the output with the static prefix
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
      print prefix "/" suf ":" t
    }
  }
' "$input_file" > "$static_tmp"

# Merge static items that are not in the dynamic list
echo "Merging static images…"
while IFS= read -r img; do
  [[ -z "$img" ]] && continue
  key="${img%%:*}"
  if [[ -z "${seen[$key]+_}" ]]; then
    echo "$img" >> "$out"
  fi
done < "$static_tmp"

# Clean up temporary file
rm "$static_tmp"

# Display the generated output
echo
echo "Generated $out:"
cat "$out"
