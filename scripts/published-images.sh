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

# Prints, as a JSON list, every image that .github/workflows/publish.yml publishes for a tag, with
# the compressed size of its linux/amd64 image.
#
# Usage: published-images.sh <tag>, for example published-images.sh main-ca52644
#
# The list comes from the same sources as the publish workflow: the ko build in scripts/buildko.sh,
# and the container_name and tag_suffix of the Docker build matrix in publish.yml. Each entry has
# name, ref (the image reference), category (a code scanning category) and size_mb.
# The script fails when an image does not exist for the tag. Run it from the repository root, with
# curl, jq and yq on the PATH.

set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <tag>" >&2
  exit 2
fi

tag=$1
registry=ghcr.io
repository=nvidia/nvsentinel

# name|tag suffix for each published image. Both lists must have images. An empty list means that
# buildko.sh or publish.yml changed in a way that this script does not read.
ko_images=$(awk '/^ko build/{f=1} f' scripts/buildko.sh | grep -oE '\./[a-z0-9/_-]+' | xargs -n1 basename |
  sed 's/$/|/') || ko_images=
docker_images=$(yq '.jobs[].strategy.matrix.include[]? | select(.container_name) |
  .container_name + "|" + (.tag_suffix // "")' .github/workflows/publish.yml | sed 's|^nvsentinel/||') ||
  docker_images=
if [ -z "${ko_images}" ]; then
  echo "found no ko images in scripts/buildko.sh" >&2
  exit 1
fi
if [ -z "${docker_images}" ]; then
  echo "found no Docker images in .github/workflows/publish.yml" >&2
  exit 1
fi

# Compressed size of the linux/amd64 image of name:reference, from the registry API.
amd64_size() {
  local name=$1 reference=$2 token index digest
  token=$(curl -fsS "https://${registry}/token?scope=repository:${repository}/${name}:pull" | jq -r .token)
  index=$(curl -fsS -H "Authorization: Bearer ${token}" \
    -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
    "https://${registry}/v2/${repository}/${name}/manifests/${reference}")
  digest=$(jq -r '.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest' \
    <<< "${index}" | head -1)
  if [ -z "${digest}" ]; then
    return 1
  fi
  curl -fsS -H "Authorization: Bearer ${token}" \
    -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
    "https://${registry}/v2/${repository}/${name}/manifests/${digest}" | jq '[.layers[].size] | add'
}

entries=()
while IFS='|' read -r name suffix; do
  reference="${tag}${suffix}"
  if ! size=$(amd64_size "${name}" "${reference}"); then
    echo "${registry}/${repository}/${name}:${reference} has no linux/amd64 image" >&2
    exit 1
  fi

  entries+=("$(jq -cn --arg name "${name}${suffix}" --arg ref "${registry}/${repository}/${name}:${reference}" \
    --argjson size "${size}" \
    '{name: $name, ref: $ref, category: ("grype-image-" + $name), size_mb: ($size / 1048576 | floor)}')")
done <<< "${ko_images}"$'\n'"${docker_images}"

printf '%s\n' "${entries[@]}" | jq -cs '.'
