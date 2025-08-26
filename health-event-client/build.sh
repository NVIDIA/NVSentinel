#!/bin/bash
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

set -e

# Configuration
NVCR_CONTAINER_REPO=${NVCR_CONTAINER_REPO:-nvcr.io}
NGC_ORG=${NGC_ORG:-nv-ngc-devops}
CI_COMMIT_REF_NAME=${CI_COMMIT_REF_NAME:-$(git rev-parse --abbrev-ref HEAD)}
SAFE_REF_NAME=$(echo "$CI_COMMIT_REF_NAME" | sed 's#/#-#g')

IMAGE_NAME="${NVCR_CONTAINER_REPO}/${NGC_ORG}/nvsentinel-health-event-client"
FULL_IMAGE_NAME="${IMAGE_NAME}:${SAFE_REF_NAME}"

echo "Building health event client image..."
echo "Image: ${FULL_IMAGE_NAME}"

# Build the image using docker buildx for multi-platform support
docker context create builder 2>/dev/null || true
docker buildx create builder --driver-opt network=host --buildkitd-flags '--allow-insecure-entitlement network.host' --use 2>/dev/null || true
docker buildx build --platform linux/arm64,linux/amd64 --network=host --push -t "${FULL_IMAGE_NAME}" -f Dockerfile .

echo "Image built and pushed successfully: ${FULL_IMAGE_NAME}"
echo "Build completed successfully!"
