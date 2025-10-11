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

# Script to replace NVIDIA Docker registry with public Docker Hub
# This allows the codebase to work with public container images

# Configuration
NVIDIA_REGISTRY="dockerhub.nvidia.com"
PUBLIC_REGISTRY="docker.io"

echo "==================================================================="
echo "Docker Registry Replacement Script"
echo "==================================================================="
echo "NVIDIA registry: ${NVIDIA_REGISTRY}"
echo "Public registry: ${PUBLIC_REGISTRY}"
echo "==================================================================="

# Check if we're in the right directory
if [ ! -f "go.mod" ] && [ ! -d ".github" ]; then
    echo "Error: This script must be run from the repository root"
    exit 1
fi

# Function to replace registry in a file
replace_in_file() {
    local file="$1"

    # Check if file exists and is readable
    if [ ! -f "$file" ] || [ ! -r "$file" ]; then
        return 0
    fi

    # Use different sed syntax for macOS vs Linux
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS
        sed -i '' "s|${NVIDIA_REGISTRY}|${PUBLIC_REGISTRY}|g" "$file"
    else
        # Linux
        sed -i "s|${NVIDIA_REGISTRY}|${PUBLIC_REGISTRY}|g" "$file"
    fi
}

echo ""
echo "Step 1: Replacing Docker registry in YAML files..."
yaml_files_count=0
find . -type f \( -name "*.yaml" -o -name "*.yml" \) ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" ! -path "*/.github/*" > /tmp/docker-yaml-files.txt
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
echo "Step 4: Replacing Docker registry in documentation..."
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
echo "  - Documentation: ${doc_files_count}"
echo "==================================================================="
echo ""
echo "Note: This script modifies files in place."
echo "Review the changes before committing."
echo "==================================================================="
