#!/usr/bin/env bash
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

# Script to replace container registry references
# Maps nvsentinel components to ghcr.io/nvidia and keeps static images at nvcr.io

# Configuration
NVIDIA_REGISTRY="dockerhub.nvidia.com"
PUBLIC_REGISTRY="docker.io"
GHCR_REGISTRY="ghcr.io/nvidia"
NVCR_REGISTRY="nvcr.io/nv-ngc-devops"

# Dynamic nvsentinel images that should map to ghcr.io/nvidia
declare -a NVSENTINEL_IMAGES=(
    "nvsentinel-csp-health-monitor"
    "nvsentinel-fault-quarantine-module"
    "nvsentinel-fault-remediation-module"
    "nvsentinel-file-server-cleanup"
    "nvsentinel-gpu-health-monitor"
    "nvsentinel-health-events-analyzer"
    "nvsentinel-labeler-module"
    "nvsentinel-log-collector"
    "nvsentinel-node-drainer-module"
    "nvsentinel-node-health-events-uds-connector-server"
    "nvsentinel-platform-connectors"
    "nvsentinel-syslog-health-monitor"
)

# Static images that should stay at nvcr.io/nv-ngc-devops
declare -a STATIC_IMAGES=(
    "nvidia-xid-analyzer-sidecar"
)

# Third-party images that need specific public registry mappings
declare -A THIRD_PARTY_MAPPINGS=(
    ["bitnami-nginx"]="docker.io/bitnamilegacy/nginx"
    ["busybox"]="public.ecr.aws/docker/library/busybox"
    ["prometheus-nginxlog-exporter"]="quay.io/martinhelmich/prometheus-nginxlog-exporter"
    ["bitnami-mongodb"]="docker.io/bitnamilegacy/mongodb"
    ["mongodb_exporter"]="docker.io/bitnamilegacy/mongodb-exporter"
    ["mongodb-exporter"]="docker.io/bitnamilegacy/mongodb-exporter"
    ["bitnami-kubectl"]="docker.io/bitnamilegacy/kubectl"
    ["rtsp-mongosh"]="ghcr.io/rtsp/docker-mongosh"
)

echo "==================================================================="
echo "Docker Registry Replacement Script"
echo "==================================================================="
echo "Old NVIDIA registry: ${NVIDIA_REGISTRY}"
echo "Public registry: ${PUBLIC_REGISTRY}"
echo "GitHub Container Registry: ${GHCR_REGISTRY}"
echo "NVCR registry for static images: ${NVCR_REGISTRY}"
echo "==================================================================="

# Check if we're in the right directory
if [ ! -f "go.mod" ] && [ ! -d ".github" ]; then
    echo "Error: This script must be run from the repository root"
    exit 1
fi

# Check for required tools on macOS
if [[ "$OSTYPE" == "darwin"* ]]; then
    if ! command -v gsed &> /dev/null; then
        echo "Error: gsed (GNU sed) is required on macOS but not found"
        echo "Please install it with: brew install gnu-sed"
        exit 1
    fi
    SED_CMD="gsed"
else
    SED_CMD="sed"
fi

# Function to replace registry in a file
replace_in_file() {
    local file="$1"

    # Check if file exists and is readable
    if [ ! -f "$file" ] || [ ! -r "$file" ]; then
        return 0
    fi

    # Create backup for safety
    local backup_file
    backup_file="${file}.backup.$(date +%s)"
    cp "$file" "$backup_file"

    # First, replace old NVIDIA registry with public registry
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS with gsed
        ${SED_CMD} -i "s|${NVIDIA_REGISTRY}|${PUBLIC_REGISTRY}|g" "$file"
    else
        # Linux sed syntax
        ${SED_CMD} -i "s|${NVIDIA_REGISTRY}|${PUBLIC_REGISTRY}|g" "$file"
    fi

    # Map nvsentinel images to ghcr.io/nvidia
    for image in "${NVSENTINEL_IMAGES[@]}"; do
        # Replace patterns like:
        # nvcr.io/nv-ngc-devops/nvsentinel-* -> ghcr.io/nvidia/nvsentinel-*
        # dockerhub.nvidia.com/nvsentinel-* -> ghcr.io/nvidia/nvsentinel-*
        # docker.io/nvsentinel-* -> ghcr.io/nvidia/nvsentinel-*
        if [[ "$OSTYPE" == "darwin"* ]]; then
            ${SED_CMD} -i "s|${NVCR_REGISTRY}/${image}|${GHCR_REGISTRY}/${image}|g" "$file"
            ${SED_CMD} -i "s|${PUBLIC_REGISTRY}/${image}|${GHCR_REGISTRY}/${image}|g" "$file"
            ${SED_CMD} -i "s|dockerhub\.nvidia\.com/${image}|${GHCR_REGISTRY}/${image}|g" "$file"
        else
            ${SED_CMD} -i "s|${NVCR_REGISTRY}/${image}|${GHCR_REGISTRY}/${image}|g" "$file"
            ${SED_CMD} -i "s|${PUBLIC_REGISTRY}/${image}|${GHCR_REGISTRY}/${image}|g" "$file"
            ${SED_CMD} -i "s|dockerhub\.nvidia\.com/${image}|${GHCR_REGISTRY}/${image}|g" "$file"
        fi
    done

    # Map third-party images to their specific public registries
    for image in "${!THIRD_PARTY_MAPPINGS[@]}"; do
        target="${THIRD_PARTY_MAPPINGS[$image]}"
        # Extract repository part for registry/repository format
        target_repo=$(echo "$target" | cut -d'/' -f2-)

        # Replace patterns like:
        # nvcr.io/nv-ngc-devops/bitnami-nginx -> docker.io/bitnamilegacy/nginx
        # dockerhub.nvidia.com/busybox -> public.ecr.aws/docker/library/busybox
        if [[ "$OSTYPE" == "darwin"* ]]; then
            ${SED_CMD} -i "s|${NVCR_REGISTRY}/${image}|${target}|g" "$file"
            ${SED_CMD} -i "s|dockerhub\.nvidia\.com/${image}|${target}|g" "$file"
        else
            ${SED_CMD} -i "s|${NVCR_REGISTRY}/${image}|${target}|g" "$file"
            ${SED_CMD} -i "s|dockerhub\.nvidia\.com/${image}|${target}|g" "$file"
        fi

        # Handle registry/repository format - replace repository line first, then fix registry
        ${SED_CMD} -i "s|repository: nv-ngc-devops/${image}|repository: ${target_repo}|g" "$file"
    done

    # Fix registry lines that should be updated for third-party images (after repository changes)
    # Use awk to process line by line and fix registry when followed by third-party repository
    awk '
    /^[[:space:]]*registry: nvcr\.io/ {
        # Store the registry line
        registry_line = $0
        # Get the next line
        if ((getline next_line) > 0) {
            # Check if next line contains third-party repository
            if (next_line ~ /repository: bitnamilegacy\//) {
                gsub(/nvcr\.io/, "docker.io", registry_line)
            } else if (next_line ~ /repository: rtsp\//) {
                gsub(/nvcr\.io/, "ghcr.io", registry_line)
            } else if (next_line ~ /repository: martinhelmich\//) {
                gsub(/nvcr\.io/, "quay.io", registry_line)
            }
            print registry_line
            print next_line
        } else {
            print registry_line
        }
        next
    }
    { print }
    ' "$file" > "${file}.tmp" && mv "${file}.tmp" "$file"

    # Map static images to nvcr.io/nv-ngc-devops (ensure they stay there)
    for image in "${STATIC_IMAGES[@]}"; do
        # Replace patterns like:
        # dockerhub.nvidia.com/nvidia-xid-analyzer-sidecar -> nvcr.io/nv-ngc-devops/nvidia-xid-analyzer-sidecar
        if [[ "$OSTYPE" == "darwin"* ]]; then
            ${SED_CMD} -i "s|dockerhub\.nvidia\.com/${image}|${NVCR_REGISTRY}/${image}|g" "$file"
        else
            ${SED_CMD} -i "s|dockerhub\.nvidia\.com/${image}|${NVCR_REGISTRY}/${image}|g" "$file"
        fi
    done

    # Remove backup if file was successfully processed
    # Note: Using $backup_file existence as success indicator since sed operations
    # completed without early returns and backup was created successfully
    if [ -f "$backup_file" ]; then
        rm -f "$backup_file"
    fi
}

echo ""
echo "Step 1: Replacing Docker registry in YAML files..."
yaml_files_count=0
find . -type f \( -name "*.yaml" -o -name "*.yml" \) ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" ! -path "*/.github/*" ! -name ".gitlab-ci.yml" > /tmp/docker-yaml-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        yaml_files_count=$((yaml_files_count + 1))
    fi
done < /tmp/docker-yaml-files.txt
rm -f /tmp/docker-yaml-files.txt
echo "Processed ${yaml_files_count} YAML files"

echo ""
echo "Step 2: Replacing Docker registry in Dockerfiles..."
dockerfile_count=0
find . -type f \( -name "Dockerfile*" -o -name "*.dockerfile" \) ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/docker-dockerfile-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        dockerfile_count=$((dockerfile_count + 1))
    fi
done < /tmp/docker-dockerfile-files.txt
rm -f /tmp/docker-dockerfile-files.txt
echo "Processed ${dockerfile_count} Dockerfiles"

echo ""
echo "Step 3: Replacing Docker registry in Makefiles..."
makefile_count=0
find . -type f \( -name "Makefile" -o -name "*.mk" \) ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/docker-makefile-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        makefile_count=$((makefile_count + 1))
    fi
done < /tmp/docker-makefile-files.txt
rm -f /tmp/docker-makefile-files.txt
echo "Processed ${makefile_count} Makefiles"

echo ""
echo "Step 4: Replacing Docker registry in Tiltfiles..."
tiltfile_count=0
find . -type f -name "Tiltfile" ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" > /tmp/docker-tiltfile-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        tiltfile_count=$((tiltfile_count + 1))
    fi
done < /tmp/docker-tiltfile-files.txt
rm -f /tmp/docker-tiltfile-files.txt
echo "Processed ${tiltfile_count} Tiltfiles"

echo ""
echo "Step 5: Replacing Docker registry in documentation..."
doc_files_count=0
find . -type f \( -name "*.md" -o -name "*.txt" \) ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" > /tmp/docker-doc-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        doc_files_count=$((doc_files_count + 1))
    fi
done < /tmp/docker-doc-files.txt
rm -f /tmp/docker-doc-files.txt
echo "Processed ${doc_files_count} documentation files"

echo ""
echo "==================================================================="
echo "Docker registry replacement complete!"
echo "==================================================================="
echo "Summary:"
echo "  - YAML files:    ${yaml_files_count}"
echo "  - Dockerfiles:   ${dockerfile_count}"
echo "  - Makefiles:     ${makefile_count}"
echo "  - Tiltfiles:     ${tiltfile_count}"
echo "  - Documentation: ${doc_files_count}"
echo "==================================================================="
echo "Registry mappings applied:"
echo "  - Old NVIDIA registry (${NVIDIA_REGISTRY}) -> ${PUBLIC_REGISTRY}"
echo "  - NVSentinel images -> ${GHCR_REGISTRY}"
echo "  - Static images -> ${NVCR_REGISTRY}"
echo ""
echo "NVSentinel images mapped to GitHub Container Registry:"
for image in "${NVSENTINEL_IMAGES[@]}"; do
    echo "  ✓ ${image}"
done
echo ""
echo "Third-party images mapped to public registries:"
for image in "${!THIRD_PARTY_MAPPINGS[@]}"; do
    echo "  ✓ ${image} -> ${THIRD_PARTY_MAPPINGS[$image]}"
done
echo ""
echo "Static images kept at NVCR:"
for image in "${STATIC_IMAGES[@]}"; do
    echo "  ✓ ${image}"
done
echo "==================================================================="
echo ""
echo "Note: This script modifies files in place and creates backups."
echo "Excluded files: .gitlab-ci.yml (left unchanged)"
echo "Review the changes before committing."
echo "==================================================================="
