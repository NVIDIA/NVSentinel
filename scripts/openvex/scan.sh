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

# Scans images with Grype like the weekly image scan, without and with an OpenVEX document.
#
# Usage: scripts/openvex/scan.sh [-o DIR] [-x OPENVEX] IMAGE...
#   -o DIR      where to write the Grype output (default: a new temporary directory)
#   -x OPENVEX  the OpenVEX document (default: .openvex.json)
#   IMAGE       an image reference, for example ghcr.io/nvidia/nvsentinel/gpu-reset:main-6e388be.
#               Grype pulls it from the registry. Use docker:IMAGE for an image in the local daemon.
#
# For each image, the script prints the critical and high counts without and with the document, and
# the critical and high findings that the document does not cover, as tab-separated lines:
# vulnerability ID, package type, name, version, PURL and location.
#
# Like the weekly scan, it counts only findings that have a fix, and takes the severity from the
# CVSS score in the SARIF output, as the Security tab does. In DIR it keeps, for each image,
# NAME.json with every finding without the document (the input of statements.py prune) and
# NAME.vex.json with the findings that the document does not cover.
#
# Run it from the repository root. It needs docker, jq and yq, and uses the Grype version of
# .versions.yaml. The Grype database is kept in the Docker volume grype-db.

set -euo pipefail

usage() {
  sed -n '/^# Usage:/,/^#$/p' "$0" | sed 's/^# \{0,1\}//' >&2
  exit 2
}

out=""
vex=".openvex.json"
while getopts "o:x:h" opt; do
  case "${opt}" in
    o) out=${OPTARG} ;;
    x) vex=${OPTARG} ;;
    *) usage ;;
  esac
done
shift $((OPTIND - 1))
[ "$#" -ge 1 ] || usage

grype_version=$(yq '.container_tools.grype' .versions.yaml)
out=${out:-$(mktemp -d)}
mkdir -p "${out}"
chmod 777 "${out}"
cp "${vex}" "${out}/openvex.json"

grype() {
  docker run --rm \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v grype-db:/db -e GRYPE_DB_CACHE_DIR=/db \
    -e GRYPE_DB_AUTO_UPDATE="${GRYPE_DB_AUTO_UPDATE:-true}" \
    -v "${out}:/out" \
    "anchore/grype:${grype_version}" "$@"
}

# Prints "<critical> critical, <high> high" for a SARIF file, counting only the rules listed in the
# file given as $2 when it is not empty.
count() {
  jq -r --rawfile keep "$2" '
    ($keep | split("\n") | map(select(length > 0))) as $k
    | .runs[0] as $r
    | (reduce ($r.tool.driver.rules // [])[] as $x ({}; .[$x.id] = ($x.properties["security-severity"] | tonumber))) as $s
    | [$r.results[] | .ruleId as $id | select(($k | length) == 0 or ($k | index($id))) | $s[$id]]
    | "\(map(select(. >= 9)) | length) critical, \(map(select(. >= 7 and . < 9)) | length) high"' "$1"
}

for image in "$@"; do
  ref=${image}
  [[ "${ref}" =~ ^(registry|docker|podman|oci-archive|docker-archive|oci-dir): ]] || ref="registry:${ref}"
  name=$(printf '%s' "${image}" | tr '/:@' '___')

  grype "${ref}" -q -o "json=/out/${name}.json" -o "sarif=/out/${name}.sarif"
  grype "${ref}" -q --only-fixed --vex /out/openvex.json -o "json=/out/${name}.vex.json" -o "sarif=/out/${name}.vex.sarif"

  # SARIF rule IDs are <vulnerability>-<package>. The scan without the document has every finding,
  # so keep the ones that have a fix, as --only-fixed does.
  jq -r '.matches[] | select(.vulnerability.fix.state == "fixed") | "\(.vulnerability.id)-\(.artifact.name)"' \
    "${out}/${name}.json" | sort -u > "${out}/${name}.fixed"
  : > "${out}/${name}.all"
  jq -r '.runs[0].tool.driver.rules[] | select((.properties["security-severity"] | tonumber) >= 7) | .id' \
    "${out}/${name}.vex.sarif" | sort -u > "${out}/${name}.left"

  echo "## ${image}"
  echo "without ${vex}: $(count "${out}/${name}.sarif" "${out}/${name}.fixed")"
  echo "with ${vex}: $(count "${out}/${name}.vex.sarif" "${out}/${name}.all")"
  jq -r --rawfile r "${out}/${name}.left" '($r | split("\n") | map(select(length > 0))) as $ids
    | .matches[] | (.vulnerability.id + "-" + .artifact.name) as $k | select($ids | index($k))
    | [.vulnerability.id, .artifact.type, .artifact.name, .artifact.version, .artifact.purl,
       .artifact.locations[0].path] | @tsv' "${out}/${name}.vex.json" | sort -u
  echo
done

echo "Grype output: ${out}" >&2
