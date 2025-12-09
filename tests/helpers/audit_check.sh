#!/bin/sh
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

# audit_check.sh - Validates audit logs exist and have correct JSON format using jq.
# Exits 0 only if valid audit entries are found, exits 1 otherwise.
#
# Usage: audit_check.sh <component> <log_directory>

set -e

# Install jq for JSON validation
apk add --no-cache jq >/dev/null 2>&1

COMP="$1"
DIR="$2"

# Find audit log files for this component
FILES=$(find "$DIR" -name "*${COMP}*-audit.log" -type f 2>/dev/null)
if [ -z "$FILES" ]; then
    echo "ERROR: No audit log files found for $COMP in $DIR"
    exit 1
fi

VALID=0
for FILE in $FILES; do
    echo "Checking: $FILE"
    
    # Read last 10 lines and validate each as JSON with required fields using jq
    tail -10 "$FILE" 2>/dev/null | while IFS= read -r line; do
        [ -z "$line" ] && continue
        
        # Use jq to validate JSON and check required fields exist
        if ! echo "$line" | jq -e '. | has("timestamp") and has("method") and has("component") and has("url")' > /dev/null 2>&1; then
            echo "INVALID JSON or MISSING FIELDS: $line"
            continue
        fi
        
        # Use jq to extract method and validate it's a write operation
        METHOD=$(echo "$line" | jq -r '.method')
        case "$METHOD" in
            POST|PUT|PATCH|DELETE)
                echo "VALID: $line"
                echo "1" > /tmp/valid_found
                ;;
            *)
                echo "INVALID METHOD ($METHOD): $line"
                ;;
        esac
    done
done

# Check if we found at least one valid entry
if [ -f /tmp/valid_found ]; then
    echo "SUCCESS: Found valid audit log entries for $COMP"
    exit 0
else
    echo "ERROR: No valid audit log entries found for $COMP"
    exit 1
fi

