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

set -euo pipefail

export KO_LOG=info 
export KO_DOCKER_REPO="${KO_DOCKER_REPO,,}"
export VERSION="${VERSION:-v0.1.0}"
export GIT_COMMIT="${GIT_COMMIT:-dev}"
BUILD_DATE=$(date -u +%FT%TZ)
export BUILD_DATE

# Display build variables for debugging
echo "Build variables:"
echo "  VERSION=${VERSION}"
echo "  GIT_COMMIT=${GIT_COMMIT}"
echo "  BUILD_DATE=${BUILD_DATE}"

# Build flags - use PLATFORMS env var if set, otherwise use .ko.yaml defaults
KO_FLAGS=(-B --image-refs=digests.txt --sbom=none --tags="${VERSION}")
if [ -n "${PLATFORMS:-}" ]; then
  echo "Building for platforms: ${PLATFORMS}"
  KO_FLAGS+=(--platform="${PLATFORMS}")
fi

# Ensure go.work file exists
if [ ! -f go.work ]; then
  go work init
  go work use \
    ./fault-quarantine \
    ./fault-remediation \
    ./health-events-analyzer \
    ./health-monitors/csp-health-monitor \
    ./janitor \
    ./labeler \
    ./node-drainer \
    ./platform-connectors
fi

ko build "${KO_FLAGS[@]}" \
  ./fault-quarantine \
  ./fault-remediation \
  ./health-events-analyzer \
  ./health-monitors/csp-health-monitor/cmd/csp-health-monitor \
  ./health-monitors/csp-health-monitor/cmd/maintenance-notifier \
  ./janitor \
  ./labeler \
  ./node-drainer \
  ./platform-connectors 

echo "built refs:"
cat digests.txt

# digests.txt has: ghcr.io/nvidia/nvsentinel/fault-quarantine:v0.1.0@sha256:9168...
# for attestation matrix we need subject-name WITHOUT tag, and the digest.
jq -R -s '
  split("\n")
  | map(select(length>0))
  | map({
      name: (   split("@")[0] | sub(":[^@]+$"; "") ),
      digest: ( split("@")[1] )
    })
' digests.txt | tee images.json

# Export images.json content to GitHub Actions output
echo "images=$(jq -c . images.json)" >> "$GITHUB_OUTPUT"