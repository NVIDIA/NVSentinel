#!/usr/bin/env bash
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

# Writes a Grype configuration that adds an ignore rule for each Go vulnerability that govulncheck
# finds in a module but whose vulnerable code the module does not call.
#
# Usage: govulncheck-ignore-rules.sh <base config> <output config>
#
# Grype matches versions, so it reports a vulnerable version of a Go module even when nothing calls
# the vulnerable code. govulncheck follows the call graph, and reports a vulnerability at the module
# or package level when the code is present but not called, and at the symbol level when it is
# called. For each vulnerability that a module does not call, this script writes a rule for its ID
# and each alias, limited to the package, version and go.mod file of that module. A vulnerability
# that govulncheck does not know, or a module where govulncheck fails, gets no rule, so Grype still
# reports it.
#
# Run it from the repository root, with govulncheck, jq and yq on the PATH. It skips the go.mod files
# in the paths that the exclude list of the base configuration names.

set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <base config> <output config>" >&2
  exit 2
fi

base_config=$1
output_config=$2
repo_root=$PWD
work_dir=$(mktemp -d)
trap 'rm -rf "${work_dir}"' EXIT

# Patterns such as ./tests/** in the exclude list become the prefix ./tests/.
excluded_prefixes=()
if [ -f "${base_config}" ]; then
  while IFS= read -r pattern; do
    excluded_prefixes+=("${pattern%\*\*}")
  done < <(yq '.exclude // [] | .[]' "${base_config}")
fi

is_excluded() {
  local dir=$1
  local prefix
  for prefix in "${excluded_prefixes[@]}"; do
    if [[ "${dir}/" == "${prefix}"* ]]; then
      return 0
    fi
  done
  return 1
}

: > "${work_dir}/rules.jsonl"

while IFS= read -r go_mod; do
  module_dir=$(dirname "${go_mod}")
  if is_excluded "${module_dir}"; then
    continue
  fi

  echo "::group::govulncheck ${module_dir}"
  if ! (cd "${module_dir}" && "${repo_root}/scripts/retry.sh" govulncheck -json ./...) \
    > "${work_dir}/result.json"; then
    echo "::warning::govulncheck failed in ${module_dir}, so Grype reports all of its Go findings"
    echo "::endgroup::"
    continue
  fi

  # A vulnerability is called when one of its findings has a function in the first frame of its
  # trace. Every other vulnerability that the module requires or imports gets a rule for its ID
  # and its aliases.
  jq -c --slurp --arg location "${module_dir#.}/go.mod" '
    (map(select(.config)) | first | .config.scanner_version // "unknown") as $scanner
    | (map(select(.osv)) | map({key: .osv.id, value: (.osv.aliases // [])}) | from_entries) as $aliases
    | [.[] | select(.finding) | .finding] as $findings
    | ($findings | map(select(.trace[0].function)) | map(.osv) | unique) as $called
    | $findings
    | map(select(.osv as $id | $called | index($id) | not))
    | map({osv, module: .trace[0].module, version: .trace[0].version})
    | unique[]
    | . as $f
    | ([$f.osv] + ($aliases[$f.osv] // []))[]
    | {
        vulnerability: .,
        reason: "govulncheck \($scanner) found no call to the vulnerable code of \($f.osv) in \($location | ltrimstr("/") | rtrimstr("/go.mod"))",
        package: {name: $f.module, version: $f.version, location: $location}
      }
  ' "${work_dir}/result.json" | tee -a "${work_dir}/rules.jsonl" \
    | jq -r '"not called: \(.vulnerability) in \(.package.name)@\(.package.version)"'
  echo "::endgroup::"
done < <(find . -name go.mod -not -path './.git/*' | sort)

jq --slurp '.' "${work_dir}/rules.jsonl" > "${work_dir}/rules.json"

if [ -f "${base_config}" ]; then
  cp "${base_config}" "${output_config}"
else
  echo '{}' > "${output_config}"
fi

RULES="${work_dir}/rules.json" yq -i '.ignore = ((.ignore // []) + load(strenv(RULES)))' "${output_config}"

echo "Wrote $(jq length "${work_dir}/rules.json") ignore rules to ${output_config}"
