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

# Script to configure ctlptl registry authentication using Docker daemon registry mirrors

set -euo pipefail

# Function to extract first registry mirror and convert to ctlptl registryAuths format
generate_first_registry_auth() {
    local daemon_json_path="/etc/docker/daemon.json"

    # Check if daemon.json exists
    if [[ ! -f "$daemon_json_path" ]]; then
        echo "Warning: $daemon_json_path not found, skipping registry auth configuration" >&2
        return 1
    fi

    # Extract first registry mirror using jq
    local first_mirror
    first_mirror=$(jq -r '.["registry-mirrors"][0]? // empty' "$daemon_json_path" 2>/dev/null)

    if [[ -z "$first_mirror" ]]; then
        echo "Warning: No registry mirrors found in $daemon_json_path, skipping registry auth configuration" >&2
        return 1
    fi

    echo "registryAuths:"
    echo "- host: docker.io"
    echo "  endpoint: $first_mirror"
}

# Function to update existing ctlptl config with first registry auth
update_ctlptl_config() {
    local ctlptl_config_path="${1:-ctlptl-config.yaml}"
    local temp_auth
    local temp_file

    # Generate the registryAuths section
    temp_auth=$(generate_first_registry_auth)

    if [[ -z "$temp_auth" ]]; then
        echo "No registry auth to add, keeping existing config"
        return 0
    fi

    # Check if ctlptl config exists
    if [[ ! -f "$ctlptl_config_path" ]]; then
        echo "Error: $ctlptl_config_path not found" >&2
        return 1
    fi

    # Create temporary file for updated config
    temp_file=$(mktemp)

    # Read existing config and insert registryAuths after registry line
    awk -v auth="$temp_auth" '
    /^registry:/ {
        print $0
        print auth
        next
    }
    # Skip existing registryAuths section if it exists
    /^registryAuths:/ {
        while (getline && /^[ \t]*-/) {
            # Skip existing auth entries
        }
        # Print the current line (which is not an auth entry)
        print $0
        next
    }
    { print }
    ' "$ctlptl_config_path" > "$temp_file"

    # Replace original file with updated config
    mv "$temp_file" "$ctlptl_config_path"

    echo "Updated $ctlptl_config_path with registry authentication from Docker daemon configuration"
}

# Main execution
main() {
    local ctlptl_config_path="${1:-ctlptl-config.yaml}"

    echo "Configuring ctlptl registry authentication..."

    # Check if jq is available
    if ! command -v jq &> /dev/null; then
        echo "Error: jq is required but not installed" >&2
        exit 1
    fi

    update_ctlptl_config "$ctlptl_config_path"
}

# Run main function with all arguments
main "$@"