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
# attest-sbom.sh - Attest SBOM to container images with retry logic
#
# Usage: attest-sbom.sh <sbom-file> <image-name> <image-digest>
#
# Environment variables:
#   MAX_RETRIES: Number of retry attempts (default: 3)
#   RETRY_DELAY: Seconds between retries (default: 5)
#   COSIGN_EXPERIMENTAL: Enable experimental cosign features (default: 1)

set -euo pipefail

# Configuration
MAX_RETRIES="${MAX_RETRIES:-3}"
RETRY_DELAY="${RETRY_DELAY:-5}"
export COSIGN_EXPERIMENTAL="${COSIGN_EXPERIMENTAL:-1}"

# Validate arguments
if [ $# -ne 3 ]; then
  echo "Usage: $0 <sbom-file> <image-name> <image-digest>" >&2
  echo "Example: $0 sbom.cdx.json ghcr.io/nvidia/nvsentinel/module sha256:abc123..." >&2
  exit 1
fi

SBOM_FILE="$1"
IMAGE_NAME="$2"
IMAGE_DIGEST="$3"
IMAGE_REF="${IMAGE_NAME}@${IMAGE_DIGEST}"

# Validate SBOM file exists
if [ ! -f "$SBOM_FILE" ]; then
  echo "::error::SBOM file not found: $SBOM_FILE" >&2
  exit 1
fi

# Function to attest with retry logic
attest_with_retry() {
  local target_ref="$1"
  local platform_info="${2:-unknown}"
  local attempt=1
  
  while [ $attempt -le "$MAX_RETRIES" ]; do
    echo "Attesting ${target_ref} (${platform_info}) - attempt ${attempt}/${MAX_RETRIES}"
    
    # Run cosign attest and capture both stdout and stderr, plus exit code
    set +e  # Temporarily disable exit on error to capture output
    cosign attest \
      --yes \
      --predicate "$SBOM_FILE" \
      --type cyclonedx \
      "$target_ref" > /tmp/cosign_output.log 2>&1
    local exit_code=$?
    set -e  # Re-enable exit on error
    
    # Show the output
    cat /tmp/cosign_output.log
    
    # Check if attestation succeeded
    if [ $exit_code -eq 0 ]; then
      echo "✓ Attestation successful for ${target_ref} (exit code: 0)"
      
      # Additional verification: check if attestation exists in registry
      sleep 2  # Brief delay for registry propagation
      if cosign verify-attestation \
        --type cyclonedx \
        --certificate-identity-regexp=".*" \
        --certificate-oidc-issuer-regexp=".*" \
        "$target_ref" &>/dev/null; then
        echo "✓ Attestation verified in registry for ${target_ref}"
        return 0
      else
        echo "⚠ Attestation created but not yet visible in registry, continuing anyway"
        return 0
      fi
    fi
    
    # If we get here, attestation failed
    echo "✗ Attestation attempt ${attempt} failed for ${target_ref} (exit code: ${exit_code})"
    echo "=== Cosign output ==="
    cat /tmp/cosign_output.log || true
    echo "=== End of cosign output ==="
    
    if [ $attempt -lt "$MAX_RETRIES" ]; then
      echo "Retrying in ${RETRY_DELAY} seconds..."
      sleep "$RETRY_DELAY"
      attempt=$((attempt + 1))
    else
      echo "::error::Failed to attest ${target_ref} after ${MAX_RETRIES} attempts"
      echo "::error::Last exit code: ${exit_code}"
      echo "::error::Last output:"
      cat /tmp/cosign_output.log || true
      return 1
    fi
  done
}

# Check if this is a multi-platform image (OCI index)
echo "Checking manifest type for ${IMAGE_REF}..."
MANIFEST_TYPE=$(crane manifest "$IMAGE_REF" | jq -r '.mediaType // "unknown"')

if [[ "$MANIFEST_TYPE" == "application/vnd.oci.image.index.v1+json" ]] || \
   [[ "$MANIFEST_TYPE" == "application/vnd.docker.distribution.manifest.list.v2+json" ]]; then
  # Multi-platform: attest each platform digest separately
  echo "Detected multi-platform image, will attest each platform separately"
  
  # Get platform digests with architecture info
  PLATFORM_INFO=$(crane manifest "$IMAGE_REF" | \
    jq -r '.manifests[] | select((.annotations."vnd.docker.reference.type" // "") != "attestation-manifest") | "\(.digest) \(.platform.architecture)/\(.platform.os)"')
  
  FAILED_PLATFORMS=()
  while IFS= read -r line; do
    DIGEST=$(echo "$line" | awk '{print $1}')
    PLATFORM=$(echo "$line" | awk '{print $2}')
    
    if ! attest_with_retry "${IMAGE_NAME}@${DIGEST}" "$PLATFORM"; then
      FAILED_PLATFORMS+=("$PLATFORM ($DIGEST)")
    fi
  done <<< "$PLATFORM_INFO"
  
  # Check if any platforms failed
  if [ ${#FAILED_PLATFORMS[@]} -gt 0 ]; then
    echo "::error::Failed to attest the following platforms:"
    printf '::error::  - %s\n' "${FAILED_PLATFORMS[@]}"
    exit 1
  fi
  
  echo "✓ All platform attestations completed successfully"
else
  # Single-platform: attest directly
  echo "Detected single-platform image"
  attest_with_retry "$IMAGE_REF" "single-platform"
fi

echo "✓ SBOM attestation process completed"
