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

# Script to replace GitLab imports with GitHub imports
# This allows the codebase to work on both GitLab and GitHub

# Configuration
GITLAB_BASE_PATH="gitlab-master.nvidia.com/dgxcloud/mk8s"
GITLAB_IMPORT="gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel"
DEFAULT_GITHUB_BASE_PATH="github.com/nvidia"
DEFAULT_GITHUB_REPO="github.com/nvidia/nvsentinel"

# Determine the target GitHub repository and base path
# Use GITHUB_REPOSITORY env var if available (format: owner/repo)
# Otherwise use the default
if [ -n "${GITHUB_REPOSITORY:-}" ]; then
    # Extract owner from GITHUB_REPOSITORY (e.g., "nvidia/nvsentinel" -> "nvidia")
    GITHUB_OWNER=$(echo "${GITHUB_REPOSITORY}" | cut -d'/' -f1)
    GITHUB_BASE_PATH="github.com/${GITHUB_OWNER}"
    GITHUB_IMPORT="github.com/${GITHUB_REPOSITORY}"
else
    GITHUB_BASE_PATH="${DEFAULT_GITHUB_BASE_PATH}"
    GITHUB_IMPORT="${DEFAULT_GITHUB_REPO}"
fi

echo "==================================================================="
echo "Import Replacement Script"
echo "==================================================================="
echo "GitLab base path: ${GITLAB_BASE_PATH}"
echo "GitHub base path: ${GITHUB_BASE_PATH}"
echo "GitLab import:    ${GITLAB_IMPORT}"
echo "GitHub import:    ${GITHUB_IMPORT}"
echo "==================================================================="

# Check if we're in the right directory
if [ ! -f "go.mod" ] && [ ! -d ".github" ]; then
    echo "Error: This script must be run from the repository root"
    exit 1
fi

# Function to replace imports in a file
replace_in_file() {
    local file="$1"

    # Check if file exists and is readable
    if [ ! -f "$file" ] || [ ! -r "$file" ]; then
        return 0
    fi

    # Use different sed syntax for macOS vs Linux
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS
        sed -i '' "s|${GITLAB_IMPORT}|${GITHUB_IMPORT}|g" "$file"
    else
        # Linux
        sed -i "s|${GITLAB_IMPORT}|${GITHUB_IMPORT}|g" "$file"
    fi
}

# Function to replace base path in files (for GOPRIVATE, etc.)
# This replaces gitlab-master.nvidia.com/dgxcloud/mk8s with github.com/nvidia
replace_base_path_in_file() {
    local file="$1"

    # Check if file exists and is readable
    if [ ! -f "$file" ] || [ ! -r "$file" ]; then
        return 0
    fi

    # Use different sed syntax for macOS vs Linux
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS
        sed -i '' "s|${GITLAB_BASE_PATH}|${GITHUB_BASE_PATH}|g" "$file"
    else
        # Linux
        sed -i "s|${GITLAB_BASE_PATH}|${GITHUB_BASE_PATH}|g" "$file"
    fi
}

# Function to replace standalone domain references (for GITLAB_HOST build args)
# This replaces gitlab-master.nvidia.com with github.com
# But avoids replacing in sed commands or other shell commands
replace_domain_in_file() {
    local file="$1"

    # Check if file exists and is readable
    if [ ! -f "$file" ] || [ ! -r "$file" ]; then
        return 0
    fi

    # Only do domain replacement in Makefiles for specific contexts:
    # - GOPRIVATE=gitlab-master.nvidia.com
    # - GITLAB_HOST=gitlab-master.nvidia.com
    # - git@gitlab-master.nvidia.com
    # - url.\"https://...@gitlab-master.nvidia.com
    # Don't replace in sed commands or other quoted contexts

    # Use different sed syntax for macOS vs Linux
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS
        sed -i '' \
            -e "s|GOPRIVATE=gitlab-master\.nvidia\.com|GOPRIVATE=github.com|g" \
            -e "s|GITLAB_HOST=gitlab-master\.nvidia\.com|GITLAB_HOST=github.com|g" \
            -e "s|git@gitlab-master\.nvidia\.com|git@github.com|g" \
            -e "s|https://oauth2:\${[^}]*}@gitlab-master\.nvidia\.com|https://oauth2:\${GITLAB_TOKEN}@github.com|g" \
            -e "s|https://gitlab-master\.nvidia\.com|https://github.com|g" \
            "$file"
    else
        # Linux
        sed -i \
            -e "s|GOPRIVATE=gitlab-master\.nvidia\.com|GOPRIVATE=github.com|g" \
            -e "s|GITLAB_HOST=gitlab-master\.nvidia\.com|GITLAB_HOST=github.com|g" \
            -e "s|git@gitlab-master\.nvidia\.com|git@github.com|g" \
            -e "s|https://oauth2:\${[^}]*}@gitlab-master\.nvidia\.com|https://oauth2:\${GITLAB_TOKEN}@github.com|g" \
            -e "s|https://gitlab-master\.nvidia\.com|https://github.com|g" \
            "$file"
    fi
}

echo ""
echo "Step 1: Replacing imports in Go files..."
go_files_count=0
find . -type f -name "*.go" ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" > /tmp/go-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        go_files_count=$((go_files_count + 1))
    fi
done < /tmp/go-files.txt
echo "Processed ${go_files_count} Go files"

echo ""
echo "Step 1a: Running gofmt on modified Go files..."
gofmt_count=0
while IFS= read -r file; do
    if [ -n "$file" ]; then
        gofmt -w "$file"
        gofmt_count=$((gofmt_count + 1))
    fi
done < /tmp/go-files.txt
echo "Formatted ${gofmt_count} Go files with gofmt"

echo ""
echo "Step 1b: Running goimports on modified Go files..."
goimports_count=0
# Check if goimports is available
if command -v goimports &> /dev/null; then
    while IFS= read -r file; do
        if [ -n "$file" ]; then
            goimports -w "$file"
            goimports_count=$((goimports_count + 1))
        fi
    done < /tmp/go-files.txt
    echo "Formatted ${goimports_count} Go files with goimports"
else
    echo "Warning: goimports not found, skipping. Install with: go install golang.org/x/tools/cmd/goimports@latest"
fi
rm -f /tmp/go-files.txt

echo ""
echo "Step 2: Replacing module paths in go.mod files..."
gomod_files_count=0
find . -type f -name "go.mod" ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/gomod-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        gomod_files_count=$((gomod_files_count + 1))
    fi
done < /tmp/gomod-files.txt
rm -f /tmp/gomod-files.txt
echo "Processed ${gomod_files_count} go.mod files"

echo ""
echo "Step 3: Replacing imports in go.sum files..."
gosum_files_count=0
find . -type f -name "go.sum" ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/gosum-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        gosum_files_count=$((gosum_files_count + 1))
    fi
done < /tmp/gosum-files.txt
rm -f /tmp/gosum-files.txt
echo "Processed ${gosum_files_count} go.sum files"

echo ""
echo "Step 4: Replacing references in documentation..."
doc_files_count=0
find . -type f \( -name "*.md" -o -name "*.txt" \) ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" > /tmp/doc-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"             # 1. Replace full nvsentinel path (specific)
        replace_base_path_in_file "$file"   # 2. Replace base path for other repos
        replace_domain_in_file "$file"      # 3. Replace standalone domain references
        doc_files_count=$((doc_files_count + 1))
    fi
done < /tmp/doc-files.txt
rm -f /tmp/doc-files.txt
echo "Processed ${doc_files_count} documentation files"

echo ""
echo "Step 5: Replacing references in YAML files..."
yaml_files_count=0
find . -type f \( -name "*.yaml" -o -name "*.yml" \) ! -path "*/vendor/*" ! -path "*/.git/*" ! -path "*/\.venv/*" ! -path "*/.github/*" > /tmp/yaml-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        yaml_files_count=$((yaml_files_count + 1))
    fi
done < /tmp/yaml-files.txt
rm -f /tmp/yaml-files.txt
echo "Processed ${yaml_files_count} YAML files"

echo ""
echo "Step 6: Replacing references in Dockerfiles..."
dockerfile_count=0
find . -type f \( -name "Dockerfile*" -o -name "*.dockerfile" \) ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/dockerfile-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        dockerfile_count=$((dockerfile_count + 1))
    fi
done < /tmp/dockerfile-files.txt
rm -f /tmp/dockerfile-files.txt
echo "Processed ${dockerfile_count} Dockerfiles"

echo ""
echo "Step 7: Replacing import paths and base path references in Makefiles..."
makefile_count=0
find . -type f \( -name "Makefile" -o -name "*.mk" \) ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/makefile-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"             # 1. Replace full nvsentinel path (specific)
        replace_base_path_in_file "$file"   # 2. Replace base path for other repos
        replace_domain_in_file "$file"      # 3. Replace standalone domain (GITLAB_HOST, etc.)
        makefile_count=$((makefile_count + 1))
    fi
done < /tmp/makefile-files.txt
rm -f /tmp/makefile-files.txt
echo "Processed ${makefile_count} Makefiles"

echo ""
echo "Step 7a: Replacing references in Protocol Buffer files..."
proto_files_count=0
find . -type f -name "*.proto" ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/proto-files.txt
while IFS= read -r file; do
    if [ -n "$file" ]; then
        replace_in_file "$file"
        proto_files_count=$((proto_files_count + 1))
    fi
done < /tmp/proto-files.txt
rm -f /tmp/proto-files.txt
echo "Processed ${proto_files_count} Protocol Buffer files"

echo ""
echo "Step 8: Adding replace directives for internal modules..."
replace_count=0
find . -type f -name "go.mod" ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/gomod-replace.txt
while IFS= read -r gomod; do
    if [ -n "$gomod" ]; then
        module_dir=$(dirname "$gomod")
        echo "  Adding replace directives in ${module_dir}..."

        # Calculate relative path to repo root
        depth=$(echo "$module_dir" | tr -cd '/' | wc -c)
        if [ "$depth" -eq 0 ]; then
            rel_path="./"
        else
            rel_path=$(printf '../%.0s' $(seq 1 "$depth"))
        fi

        # Add replace directives for all internal modules
        (cd "$module_dir" && {
            # Remove existing replace directives for our modules first
            if [[ "$OSTYPE" == "darwin"* ]]; then
                sed -i '' '\|replace.*'"${GITHUB_IMPORT}"'|d' go.mod
            else
                sed -i '\|replace.*'"${GITHUB_IMPORT}"'|d' go.mod
            fi

            # Add replace directives
            {
                echo ""
                echo "// Local replacements for internal modules"
                echo "replace ${GITHUB_IMPORT}/statemanager => ${rel_path}statemanager"
                echo "replace ${GITHUB_IMPORT}/health-monitors/csp-health-monitor => ${rel_path}health-monitors/csp-health-monitor"
                echo "replace ${GITHUB_IMPORT}/health-monitors/nvswitch-health-monitor => ${rel_path}health-monitors/nvswitch-health-monitor"
                echo "replace ${GITHUB_IMPORT}/health-monitors/nic-health-monitor => ${rel_path}health-monitors/nic-health-monitor"
                echo "replace ${GITHUB_IMPORT}/health-monitors/syslog-health-monitor => ${rel_path}health-monitors/syslog-health-monitor"
                echo "replace ${GITHUB_IMPORT}/platform-connectors => ${rel_path}platform-connectors"
                echo "replace ${GITHUB_IMPORT}/store-client-sdk => ${rel_path}store-client-sdk"
                echo "replace ${GITHUB_IMPORT}/health-event-client => ${rel_path}health-event-client"
                echo "replace ${GITHUB_IMPORT}/health-events-analyzer => ${rel_path}health-events-analyzer"
                echo "replace ${GITHUB_IMPORT}/fault-quarantine-module => ${rel_path}fault-quarantine-module"
                echo "replace ${GITHUB_IMPORT}/labeler-module => ${rel_path}labeler-module"
                echo "replace ${GITHUB_IMPORT}/node-drainer-module => ${rel_path}node-drainer-module"
                echo "replace ${GITHUB_IMPORT}/fault-remediation-module => ${rel_path}fault-remediation-module"
            } >> go.mod
        })
        replace_count=$((replace_count + 1))
    fi
done < /tmp/gomod-replace.txt
rm -f /tmp/gomod-replace.txt
echo "Added replace directives to ${replace_count} modules"

echo ""
echo "Step 9: Running go mod tidy in all modules..."
modules_tidied=0
find . -type f -name "go.mod" ! -path "*/vendor/*" ! -path "*/.git/*" > /tmp/gomod-tidy.txt
while IFS= read -r gomod; do
    if [ -n "$gomod" ]; then
        module_dir=$(dirname "$gomod")
        echo "  Tidying ${module_dir}..."
        (cd "$module_dir" && GOTOOLCHAIN=local go mod tidy) || {
            echo "    Warning: go mod tidy failed in ${module_dir}"
        }
        modules_tidied=$((modules_tidied + 1))
    fi
done < /tmp/gomod-tidy.txt
rm -f /tmp/gomod-tidy.txt
echo "Tidied ${modules_tidied} modules"

echo ""
echo "==================================================================="
echo "Import replacement complete!"
echo "==================================================================="
echo "Summary:"
echo "  - Go files:           ${go_files_count}"
echo "  - Go files formatted: ${gofmt_count} (gofmt), ${goimports_count} (goimports)"
echo "  - go.mod files:       ${gomod_files_count}"
echo "  - go.sum files:       ${gosum_files_count}"
echo "  - Documentation:      ${doc_files_count}"
echo "  - YAML files:         ${yaml_files_count}"
echo "  - Dockerfiles:        ${dockerfile_count}"
echo "  - Makefiles:          ${makefile_count}"
echo "  - Proto files:        ${proto_files_count}"
echo "  - Replace directives: ${replace_count} modules"
echo "  - Modules tidied:     ${modules_tidied}"
echo "==================================================================="
echo ""
echo "Note: This script modifies files in place."
echo "Review the changes before committing."
echo "==================================================================="
