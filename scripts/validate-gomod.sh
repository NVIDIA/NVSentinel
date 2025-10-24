#!/usr/bin/env bash

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

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Script directory and repository root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# Counters
TOTAL_MODULES=0
MODULES_WITH_ISSUES=0
TOTAL_ISSUES=0
TOTAL_TIDY_ISSUES=0

# Function to print colored output
print_error() {
    echo -e "${RED}ERROR:${NC} $1" >&2
}

print_warning() {
    echo -e "${YELLOW}WARNING:${NC} $1" >&2
}

print_success() {
    echo -e "${GREEN}SUCCESS:${NC} $1"
}

print_info() {
    echo -e "$1"
}

# Function to get relative path from one directory to another
get_relative_path() {
    local from="$1"
    local to="$2"

    # Ensure both paths are absolute
    local abs_from abs_to
    abs_from=$(cd "$from" && pwd) || {
        echo "Error: Cannot access directory '$from'" >&2
        return 1
    }
    abs_to=$(cd "$to" && pwd) || {
        echo "Error: Cannot access directory '$to'" >&2
        return 1
    }

    # Try realpath --relative-to first (GNU coreutils on Linux)
    if command -v realpath >/dev/null 2>&1; then
        if realpath --relative-to="$abs_from" "$abs_to" 2>/dev/null; then
            return 0
        fi
    fi

    # Try python3 fallback
    if command -v python3 >/dev/null 2>&1; then
        if python3 -c "import os.path; print(os.path.relpath('$abs_to', '$abs_from'))" 2>/dev/null; then
            return 0
        fi
    fi

    # Try perl fallback
    if command -v perl >/dev/null 2>&1; then
        if perl -e "use File::Spec; print File::Spec->abs2rel('$abs_to', '$abs_from')" 2>/dev/null; then
            return 0
        fi
    fi

    # Manual calculation as last resort
    # Simple case: to is a subdirectory of from
    if [[ "$abs_to" == "$abs_from"/* ]]; then
        echo "${abs_to#"$abs_from"/}"
        return 0
    fi

    # Same directory
    if [[ "$abs_to" == "$abs_from" ]]; then
        echo "."
        return 0
    fi

    # Calculate relative path manually
    local common_part="$abs_from"
    local result=""

    while [[ "$abs_to" != "$common_part"* ]]; do
        common_part=$(dirname "$common_part")
        result="../$result"
    done

    if [[ "$abs_to" == "$common_part" ]]; then
        echo "${result%/}"
    else
        echo "${result}${abs_to#"$common_part"/}"
    fi
}

# Function to check if go mod tidy is needed
check_mod_tidy() {
    local gomod_file="$1"
    local gomod_dir
    gomod_dir="$(dirname "$gomod_file")"
    local tidy_needed=0

    # Change to the directory containing go.mod
    cd "$gomod_dir"

    # Create temporary copies of go.mod and go.sum for comparison
    local temp_dir
    temp_dir=$(mktemp -d)
    trap 'rm -rf "$temp_dir"' EXIT

    # Copy current files
    cp go.mod "$temp_dir/go.mod.orig" 2>/dev/null || true
    cp go.sum "$temp_dir/go.sum.orig" 2>/dev/null || touch "$temp_dir/go.sum.orig"

    # Run go mod tidy
    if go mod tidy 2>/dev/null; then
        # Compare files to see if anything changed
        if ! cmp -s go.mod "$temp_dir/go.mod.orig" 2>/dev/null; then
            print_warning "  ⚠ go.mod has changes after 'go mod tidy'"
            tidy_needed=1
        fi

        if ! cmp -s go.sum "$temp_dir/go.sum.orig" 2>/dev/null; then
            print_warning "  ⚠ go.sum has changes after 'go mod tidy'"
            tidy_needed=1
        fi

        # Restore original files
        cp "$temp_dir/go.mod.orig" go.mod 2>/dev/null || true
        cp "$temp_dir/go.sum.orig" go.sum 2>/dev/null || true
    else
        print_error "  ✗ 'go mod tidy' failed - module may have dependency issues"
        tidy_needed=1
    fi

    # Clean up
    rm -rf "$temp_dir"
    trap - EXIT

    if [[ $tidy_needed -eq 1 ]]; then
        print_error "  ✗ Module needs 'go mod tidy' to be run"
        TOTAL_TIDY_ISSUES=$((TOTAL_TIDY_ISSUES + 1))
        return 1
    else
        print_info "  ✓ Module is tidy"
        return 0
    fi
}

# Function to validate a single go.mod file
validate_gomod() {
    local gomod_file="$1"
    local gomod_dir
    gomod_dir="$(dirname "$gomod_file")"
    local issues_found=0

    print_info "Checking ${gomod_file#"$REPO_ROOT"/}..."

    # Change to the directory containing go.mod
    if ! cd "$gomod_dir" 2>/dev/null; then
        print_error "  ✗ Cannot access directory: $gomod_dir"
        return 1
    fi

    # Get all required dependencies
    local required_deps
    if ! required_deps=$(go list -m -f '{{.Path}}' all 2>/dev/null | grep "^github.com/nvidia/nvsentinel/" || true); then
        print_error "  ✗ Failed to list module dependencies"
        issues_found=$((issues_found + 1))
        TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
        return 1
    fi

    if [[ -z "$required_deps" ]]; then
        print_info "  ✓ No local nvsentinel dependencies found"
        # Still check go mod tidy for modules without local dependencies
        local tidy_issues=0
        if ! check_mod_tidy "$gomod_file"; then
            tidy_issues=$((tidy_issues + 1))
            MODULES_WITH_ISSUES=$((MODULES_WITH_ISSUES + 1))
        fi
        return $tidy_issues
    fi

    # Get current replace directives (handle both single and grouped formats)
    local replace_directives
    replace_directives=$(go mod edit -print | awk '
        /^replace \(/ { in_replace=1; next }
        /^\)/ && in_replace { in_replace=0; next }
        /^replace / && !in_replace { print $2 }
        in_replace && /^[[:space:]]*[^[:space:]]/ { print $1 }
    ' || true)

    # Check for circular self-references in replace directives
    local current_module
    if ! current_module=$(go list -m 2>/dev/null); then
        print_error "  ✗ Failed to get current module name"
        issues_found=$((issues_found + 1))
        TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
    else
        # Check if current module replaces itself
        if echo "$replace_directives" | grep -q "^$current_module$"; then
            print_error "  ✗ Circular self-reference detected: $current_module replaces itself"
            print_error "    Remove: replace $current_module => ..."
            print_error "    Modules should not replace themselves"
            issues_found=$((issues_found + 1))
            TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
        else
            print_info "  ✓ No circular self-references found"
        fi
    fi

    # Check each required dependency
    while IFS= read -r dep; do
        [[ -z "$dep" ]] && continue

        # Skip the module itself
        local current_module
        if ! current_module=$(go list -m 2>/dev/null); then
            print_error "  ✗ Failed to get current module name"
            issues_found=$((issues_found + 1))
            TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
            continue
        fi
        if [[ "$dep" == "$current_module" ]]; then
            continue
        fi

        # Check if this dependency exists as a local module
        local dep_path="${dep#github.com/nvidia/nvsentinel/}"
        local local_dep_dir="$REPO_ROOT/$dep_path"

        if [[ -d "$local_dep_dir" && -f "$local_dep_dir/go.mod" ]]; then
            # This is a local module, check if replace directive exists
            if echo "$replace_directives" | grep -q "^$dep$"; then
                # Replace directive exists, verify it points to the correct local path
                local current_replace
                current_replace=$(go mod edit -print | awk -v dep="$dep" '
                    /^replace \(/ { in_replace=1; next }
                    /^\)/ && in_replace { in_replace=0; next }
                    /^replace / && !in_replace && $2 == dep { print $4; exit }
                    in_replace && $1 == dep { print $3; exit }
                ' || true)

                if [[ -n "$current_replace" ]]; then
                    # Convert relative path to absolute for comparison
                    local expected_relative_path
                    if ! expected_relative_path=$(get_relative_path "$gomod_dir" "$local_dep_dir"); then
                        print_error "  ✗ Failed to calculate relative path for $dep"
                        issues_found=$((issues_found + 1))
                        TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
                        continue
                    fi

                    # Normalize paths for comparison (remove ./ prefix)
                    current_replace=${current_replace#./}
                    expected_relative_path=${expected_relative_path#./}

                    if [[ "$current_replace" == "$expected_relative_path" ]]; then
                        print_info "  ✓ $dep => $current_replace"
                    else
                        print_error "  ✗ $dep has incorrect replace path:"
                        print_error "    Current: $current_replace"
                        print_error "    Expected: $expected_relative_path"
                        issues_found=$((issues_found + 1))
                        TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
                    fi
                else
                    print_error "  ✗ $dep has malformed replace directive"
                    issues_found=$((issues_found + 1))
                    TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
                fi
            else
                # Missing replace directive
                local expected_relative_path
                if ! expected_relative_path=$(get_relative_path "$gomod_dir" "$local_dep_dir"); then
                    print_error "  ✗ Failed to calculate relative path for missing replace directive: $dep"
                    issues_found=$((issues_found + 1))
                    TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
                    continue
                fi
                print_error "  ✗ Missing replace directive for $dep"
                print_error "    Add: replace $dep => $expected_relative_path"
                issues_found=$((issues_found + 1))
                TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
            fi
        fi

        # Check version for local dependencies - should be exactly v0.0.0
        if [[ -d "$local_dep_dir" && -f "$local_dep_dir/go.mod" ]]; then
            local current_version
            current_version=$(go list -m -f '{{.Version}}' "$dep" 2>/dev/null || true)
            if [[ -n "$current_version" ]]; then
                if [[ "$current_version" == "v0.0.0" ]]; then
                    print_info "  ✓ $dep version: $current_version"
                else
                    print_error "  ✗ $dep has incorrect version:"
                    print_error "    Current: $current_version"
                    print_error "    Expected: v0.0.0"
                    print_error "    Local modules should use v0.0.0 (no timestamp)"
                    issues_found=$((issues_found + 1))
                    TOTAL_ISSUES=$((TOTAL_ISSUES + 1))
                fi
            else
                print_warning "  ⚠ Cannot determine version for $dep"
            fi
        fi
    done <<< "$required_deps"

    # Check if go mod tidy is needed
    local tidy_issues=0
    if ! check_mod_tidy "$gomod_file"; then
        tidy_issues=$((tidy_issues + 1))
        if [[ $issues_found -eq 0 ]]; then
            MODULES_WITH_ISSUES=$((MODULES_WITH_ISSUES + 1))
        fi
    fi

    # Summary for this module
    local total_module_issues=$((issues_found + tidy_issues))
    if [[ $total_module_issues -eq 0 ]]; then
        print_info "  ✓ All validations passed"
    else
        if [[ $issues_found -eq 0 && $tidy_issues -gt 0 ]]; then
            MODULES_WITH_ISSUES=$((MODULES_WITH_ISSUES + 1))
        fi
    fi

    return $total_module_issues
}

# Main function
main() {
    print_info "Validating go.mod files in nvsentinel monorepo..."
    print_info "Repository root: $REPO_ROOT"
    print_info ""

    # Find all go.mod files
    local gomod_files_list
    gomod_files_list=$(find "$REPO_ROOT" -name "go.mod" -type f | sort)
    local gomod_files=()
    while IFS= read -r file; do
        [[ -n "$file" ]] && gomod_files+=("$file")
    done <<< "$gomod_files_list"

    if [[ ${#gomod_files[@]} -eq 0 ]]; then
        print_error "No go.mod files found in repository"
        exit 1
    fi

    print_info "Found ${#gomod_files[@]} go.mod files"
    print_info ""

    # Validate each go.mod file
    for gomod_file in "${gomod_files[@]}"; do
        TOTAL_MODULES=$((TOTAL_MODULES + 1))
        validate_gomod "$gomod_file"
        print_info ""
    done

    # Print summary
    print_info "========================================"
    print_info "VALIDATION SUMMARY"
    print_info "========================================"
    print_info "Total modules checked: $TOTAL_MODULES"

    if [[ $TOTAL_ISSUES -eq 0 && $TOTAL_TIDY_ISSUES -eq 0 ]]; then
        print_success "All go.mod files are valid!"
        print_info "✓ No circular self-references found"
        print_info "✓ All local module dependencies have proper replace directives"
        print_info "✓ All local module dependencies use v0.0.0 version"
        print_info "✓ All modules are properly tidied"
    else
        print_error "Validation failed!"
        print_error "Modules with issues: $MODULES_WITH_ISSUES"
        if [[ $TOTAL_ISSUES -gt 0 ]]; then
            print_error "Replace directive issues: $TOTAL_ISSUES"
        fi
        if [[ $TOTAL_TIDY_ISSUES -gt 0 ]]; then
            print_error "Modules needing 'go mod tidy': $TOTAL_TIDY_ISSUES"
        fi
        print_info ""
        print_info "To fix these issues:"
        local step=1
        if [[ $TOTAL_ISSUES -gt 0 ]]; then
            print_info "$step. Fix replace directives and version issues shown above in the respective go.mod files"
            print_info "   - Remove any circular self-references (modules replacing themselves)"
            print_info "   - Add missing replace directives as shown"
            print_info "   - Update local module versions to v0.0.0 (remove timestamps)"
            step=$((step + 1))
        fi
        if [[ $TOTAL_TIDY_ISSUES -gt 0 ]]; then
            print_info "$step. Run 'go mod tidy' in each affected module directory"
            step=$((step + 1))
        fi
        print_info "$step. Re-run this script to verify fixes"
    fi

    exit $((TOTAL_ISSUES + TOTAL_TIDY_ISSUES > 0 ? 1 : 0))
}

# Handle script arguments
case "${1:-}" in
    -h|--help)
        cat << EOF
Usage: $0 [OPTIONS]

Validates go.mod files in the nvsentinel monorepo to ensure that all local
module dependencies have proper replace directives.

OPTIONS:
    -h, --help    Show this help message

DESCRIPTION:
    This script scans all go.mod files in the repository and:
    1. Identifies dependencies on local nvsentinel modules
    2. Checks for circular self-references (modules replacing themselves)
    3. Verifies that replace directives exist for local dependencies
    4. Checks that replace paths point to the correct local directories
    5. Validates that local module dependencies use v0.0.0 version (no timestamps)
    6. Verifies that 'go mod tidy' has been run (go.mod/go.sum are clean)
    7. Reports any circular self-references
    8. Reports any missing or incorrect replace directives
    9. Reports any local modules with incorrect versions
    10. Reports any modules that need 'go mod tidy' to be run

    The script will exit with code 0 if all validations pass, or code 1 if
    any issues are found.

EXAMPLES:
    # Validate all go.mod files
    $0

    # Run from anywhere in the repository
    cd /path/to/nvsentinel/some/subdir
    scripts/validate-gomod.sh
EOF
        exit 0
        ;;
    "")
        # No arguments, proceed with validation
        ;;
    *)
        print_error "Unknown argument: $1"
        print_error "Use -h or --help for usage information"
        exit 1
        ;;
esac

# Run main function
main "$@"