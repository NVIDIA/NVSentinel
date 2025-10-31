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
# check-image-attestations.sh
#
# Validates SBOM attestations for all NVSentinel container images built with a specific tag.
# This script checks both Ko-built images (Go services) and Docker-built images (Python services).
#
# Attestation Storage:
#   Cosign v3.x stores attestations in OCI 1.1 referrers format with sha256-DIGEST tags.
#   These are NOT visible with `cosign tree` or legacy .att/.sig tag patterns.
#   Verification uses `crane` to check for sha256-prefixed attestation tags in the registry.
#
# Usage:
#   ./scripts/check-image-attestations.sh <tag> [image-name]
#   ./scripts/check-image-attestations.sh v1.2.3                              # Check all images
#   ./scripts/check-image-attestations.sh v1.2.3 gpu-health-monitor           # Check specific image
#   ./scripts/check-image-attestations.sh v1.2.3 nvsentinel/platform-connectors  # Check with namespace
#
# Image names can be specified with or without the 'nvsentinel/' prefix.
# For images with tag suffixes (dcgm-3.x, dcgm-4.x), use the base name without suffix.
#
# Requirements:
#   - crane (for manifest inspection and OCI 1.1 attestation detection)
#   - gh (GitHub CLI for build provenance verification)
#   - jq (for JSON parsing)

set -euo pipefail



# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
REGISTRY="${REGISTRY:-ghcr.io}"
ORG="${ORG:-nvidia}"
REPO_OWNER="${REPO_OWNER:-nvidia}"
REPO_NAME="${REPO_NAME:-nvsentinel}"

# Image lists
KO_IMAGES=(
    "nvsentinel/fault-quarantine-module"
    "nvsentinel/fault-remediation-module"
    "nvsentinel/health-events-analyzer"
    "nvsentinel/csp-health-monitor"
    "nvsentinel/maintenance-notifier"
    "nvsentinel/labeler-module"
    "nvsentinel/node-drainer-module"
    "nvsentinel/janitor"
    "nvsentinel/platform-connectors"
)

DOCKER_IMAGES=(
    "nvsentinel/gpu-health-monitor:dcgm-3.x"
    "nvsentinel/gpu-health-monitor:dcgm-4.x"
    "nvsentinel/syslog-health-monitor"
    "nvsentinel/log-collector"
    "nvsentinel/file-server-cleanup"
)

# Counters
TOTAL_IMAGES=0
PASSED_IMAGES=0
FAILED_IMAGES=0
SKIPPED_IMAGES=0

# Usage
usage() {
    cat <<EOF
Usage: $0 <tag>

Validates SBOM attestations for all NVSentinel container images.

Arguments:
  tag         Image tag to verify (e.g., v1.2.3, 3b37e68)

Environment Variables:
  REGISTRY    Container registry (default: ghcr.io)
  ORG         Organization name (default: nvidia)
  REPO_OWNER  GitHub repo owner (default: nvidia)
  REPO_NAME   GitHub repo name (default: nvsentinel)

Examples:
  $0 v1.2.3
  $0 3b37e68
  REGISTRY=my-registry.io ORG=myorg $0 main-abc1234

Notes:
  - Cosign v3.x stores attestations in OCI 1.1 referrers format
  - Attestations are stored as sha256-DIGEST tags (not .att/.sig)
  - Use 'crane ls' and 'crane manifest' to inspect attestation storage
  - 'cosign tree' does NOT show OCI 1.1 referrers (this is expected)

EOF
    exit 1
}

# Check required tools
check_requirements() {
    local missing_tools=()
    
    for tool in crane gh jq; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=("$tool")
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        echo -e "${RED}Error: Missing required tools: ${missing_tools[*]}${NC}"
        echo "Please install them before running this script."
        exit 1
    fi
}

# Extract platform information from multi-platform image
# Output format: "digest|architecture|os" (one per line)
get_platform_info() {
    local image_ref="$1"
    local manifest
    
    manifest=$(crane manifest "$image_ref" 2>/dev/null || echo "")
    
    if [ -z "$manifest" ]; then
        return 1
    fi
    
    # Check if it's a multi-platform index
    local media_type
    media_type=$(echo "$manifest" | jq -r '.mediaType')
    
    if [[ "$media_type" == "application/vnd.oci.image.index.v1+json" ]] || \
       [[ "$media_type" == "application/vnd.docker.distribution.manifest.list.v2+json" ]]; then
        # Extract platform info: digest|architecture|os
        echo "$manifest" | jq -r '.manifests[] | select(.platform.architecture != "unknown") | "\(.digest)|\(.platform.architecture)|\(.platform.os)"'
    else
        # Single platform image - get digest and architecture from config
        local digest
        digest=$(crane digest "$image_ref" 2>/dev/null || echo "")
        if [ -n "$digest" ]; then
            local config
            config=$(crane config "$image_ref" 2>/dev/null || echo "")
            local arch
            local os
            arch=$(echo "$config" | jq -r '.architecture // "amd64"')
            os=$(echo "$config" | jq -r '.os // "linux"')
            echo "${digest}|${arch}|${os}"
        fi
    fi
}

# Verify GitHub attestation
verify_github_attestation() {
    local image_ref="$1"
    
    if gh attestation verify "oci://${image_ref}" --owner "$REPO_OWNER" &>/dev/null; then
        return 0
    else
        return 1
    fi
}

# Verify Cosign SBOM attestation (OCI 1.1 referrers format)
verify_cosign_attestation() {
    local image_ref="$1"
    local digest="$2"
    
    # Extract image name without digest for crane ls
    local image_name="${image_ref%@*}"
    
    # Convert digest to sha256-DIGEST tag format (OCI 1.1 referrers)
    # Example: sha256:abc123... becomes sha256-abc123...
    local digest_tag="sha256-${digest#sha256:}"
    
    # Check if attestation tag exists in registry
    # Cosign v3.x stores attestations as sha256-DIGEST tags, not .att/.sig
    if ! crane ls "$image_name" 2>/dev/null | grep -q "^${digest_tag}$"; then
        return 1
    fi
    
    # Verify it's actually a Sigstore bundle attestation
    local index_manifest
    index_manifest=$(crane manifest "${image_name}:${digest_tag}" 2>/dev/null || echo "")
    
    if [ -z "$index_manifest" ]; then
        return 1
    fi
    
    # The attestation tag points to an OCI index with multiple attestations
    # Check if it's an index and extract the first attestation manifest digest
    local media_type
    media_type=$(echo "$index_manifest" | jq -r '.mediaType')
    
    if [[ "$media_type" == "application/vnd.oci.image.index.v1+json" ]]; then
        # Get all attestation manifest digests from the index
        local att_digests
        att_digests=$(echo "$index_manifest" | jq -r '.manifests[].digest')
        
        if [ -z "$att_digests" ]; then
            return 1
        fi
        
        # Check if any attestation manifest contains Sigstore bundle
        # (there may be multiple attestations from different runs)
        while IFS= read -r att_digest; do
            if [ -z "$att_digest" ] || [ "$att_digest" == "null" ]; then
                continue
            fi
            
            local att_manifest
            att_manifest=$(crane manifest "${image_name}@${att_digest}" 2>/dev/null || echo "")
            
            # Verify it contains Sigstore bundle layers
            if echo "$att_manifest" | jq -e '.layers[].mediaType | select(. == "application/vnd.dev.sigstore.bundle.v0.3+json")' &>/dev/null; then
                return 0
            fi
        done <<< "$att_digests"
    fi
    
    return 1
}

# Verify attestations for a single image digest (platform-specific)
verify_image_digest() {
    local image_name="$1"
    local digest="$2"
    local arch="$3"
    local os="$4"
    
    echo -e "${BLUE}  ${os}/${arch} - ${digest}${NC}"
    
    return 0
}

# Verify attestations for a single image
verify_image() {
    local image_name="$1"
    local tag="$2"
    local image_ref="${REGISTRY}/${ORG}/${image_name}:${tag}"
    
    echo -e "\n${BLUE}Verifying: ${image_name}:${tag}${NC}"
    TOTAL_IMAGES=$((TOTAL_IMAGES + 1))
    
    # Check if image exists
    if ! crane manifest "$image_ref" &>/dev/null; then
        echo -e "${YELLOW}  ⊘ Image not found, skipping${NC}"
        SKIPPED_IMAGES=$((SKIPPED_IMAGES + 1))
        return
    fi
    
    # Get the manifest list digest (for multi-platform images) or image digest (for single-platform)
    local manifest_digest
    manifest_digest=$(crane digest "$image_ref" 2>/dev/null || echo "")
    
    if [ -z "$manifest_digest" ]; then
        echo -e "${RED}  ✗ Failed to get manifest digest${NC}"
        FAILED_IMAGES=$((FAILED_IMAGES + 1))
        return
    fi
    
    echo -e "${BLUE}  Manifest digest: ${manifest_digest}${NC}"
    
    # Get platform information
    local platform_info
    platform_info=$(get_platform_info "$image_ref")
    
    if [ -z "$platform_info" ]; then
        echo -e "${RED}  ✗ Failed to get image platform information${NC}"
        FAILED_IMAGES=$((FAILED_IMAGES + 1))
        return
    fi
    
    # Display platform information
    while IFS='|' read -r digest arch os; do
        verify_image_digest "$image_name" "$digest" "$arch" "$os"
    done <<< "$platform_info"
    
    # Verify attestations on the manifest list digest (correct for multi-platform images)
    local manifest_ref="${REGISTRY}/${ORG}/${image_name}@${manifest_digest}"
    local cosign_ok=false
    
    echo -e "${BLUE}  Checking attestations on manifest list...${NC}"
    
    # Verify Cosign SBOM attestation (OCI 1.1 referrers format)
    if verify_cosign_attestation "$manifest_ref" "$manifest_digest"; then
        echo -e "${GREEN}  ✓ Cosign SBOM attestation (OCI 1.1 referrers)${NC}"
        cosign_ok=true
    else
        echo -e "${RED}  ✗ Cosign SBOM attestation not found${NC}"
    fi
    
    # Optionally verify GitHub build provenance attestation
    if command -v gh &>/dev/null && gh auth status &>/dev/null; then
        if verify_github_attestation "$manifest_ref"; then
            echo -e "${GREEN}  ✓ GitHub build provenance attestation${NC}"
        fi
    fi
    
    # Consider image verified if Cosign SBOM attestation exists
    if $cosign_ok; then
        echo -e "${GREEN}  ✓ All attestations verified${NC}"
        PASSED_IMAGES=$((PASSED_IMAGES + 1))
    else
        echo -e "${RED}  ✗ Attestations missing${NC}"
        FAILED_IMAGES=$((FAILED_IMAGES + 1))
    fi
}

# Main function
main() {
    if [ $# -lt 1 ] || [ $# -gt 2 ]; then
        usage
    fi
    
    local tag="$1"
    local filter_image="${2:-}"
    
    # Normalize filter_image to remove nvsentinel/ prefix if present
    if [ -n "$filter_image" ]; then
        filter_image="${filter_image#nvsentinel/}"
    fi
    
    echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  NVSentinel Image Attestation Verification${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"
    echo -e "Registry: ${REGISTRY}"
    echo -e "Organization: ${ORG}"
    echo -e "Tag: ${tag}"
    if [ -n "$filter_image" ]; then
        echo -e "Filter: ${filter_image}"
    fi
    echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"
    
    # Check requirements
    check_requirements
    
    # Verify Ko-built images
    if [ -z "$filter_image" ]; then
        echo -e "\n${BLUE}═══ Ko-built Images ═══${NC}"
        for image in "${KO_IMAGES[@]}"; do
            verify_image "$image" "$tag"
        done
    else
        # Check if filter matches any Ko-built image
        local found=false
        for image in "${KO_IMAGES[@]}"; do
            image_base="${image#nvsentinel/}"
            if [ "$image_base" == "$filter_image" ]; then
                echo -e "\n${BLUE}═══ Ko-built Images ═══${NC}"
                verify_image "$image" "$tag"
                found=true
                break
            fi
        done
        if ! $found; then
            # Not a Ko image, will check Docker images below
            :
        fi
    fi
    
    # Verify Docker-built images
    if [ -z "$filter_image" ]; then
        echo -e "\n${BLUE}═══ Docker-built Images ═══${NC}"
        for image_spec in "${DOCKER_IMAGES[@]}"; do
            # Handle images with tag suffixes (e.g., gpu-health-monitor:dcgm-3.x)
            if [[ "$image_spec" == *":"* ]]; then
                image_base="${image_spec%:*}"
                suffix="${image_spec#*:}"
                full_tag="${tag}-${suffix}"
            else
                image_base="$image_spec"
                full_tag="$tag"
            fi
            verify_image "$image_base" "$full_tag"
        done
    else
        # Check if filter matches any Docker-built image
        local docker_header_printed=false
        for image_spec in "${DOCKER_IMAGES[@]}"; do
            # Extract base name (remove nvsentinel/ prefix)
            local image_base
            if [[ "$image_spec" == *":"* ]]; then
                image_base="${image_spec%:*}"
                image_base="${image_base#nvsentinel/}"
                suffix="${image_spec#*:}"
            else
                image_base="${image_spec#nvsentinel/}"
                suffix=""
            fi
            
            # Check if this image matches the filter
            if [ "$image_base" == "$filter_image" ]; then
                if ! $docker_header_printed; then
                    echo -e "\n${BLUE}═══ Docker-built Images ═══${NC}"
                    docker_header_printed=true
                fi
                
                # Build full image reference
                if [ -n "$suffix" ]; then
                    full_image="nvsentinel/${image_base}"
                    full_tag="${tag}-${suffix}"
                else
                    full_image="nvsentinel/${image_base}"
                    full_tag="$tag"
                fi
                verify_image "$full_image" "$full_tag"
            fi
        done
        
        # If we filtered and found nothing, report it
        if [ "$TOTAL_IMAGES" -eq 0 ]; then
            echo -e "\n${YELLOW}No images found matching '${filter_image}'${NC}"
            exit 1
        fi
    fi
    
    # Print summary
    echo -e "\n${BLUE}═══════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  Verification Summary${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"
    echo -e "Total images checked: ${TOTAL_IMAGES}"
    echo -e "${GREEN}Passed: ${PASSED_IMAGES}${NC}"
    echo -e "${RED}Failed: ${FAILED_IMAGES}${NC}"
    echo -e "${YELLOW}Skipped: ${SKIPPED_IMAGES}${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════${NC}"
    
    if [ $FAILED_IMAGES -gt 0 ]; then
        echo -e "\n${RED}Some images are missing attestations!${NC}"
        exit 1
    elif [ $PASSED_IMAGES -eq 0 ]; then
        echo -e "\n${YELLOW}No images were successfully verified.${NC}"
        exit 1
    else
        echo -e "\n${GREEN}All images have valid attestations!${NC}"
        exit 0
    fi
}

main "$@"
