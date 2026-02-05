#!/usr/bin/env bash

# Copyright 2023 The Kubernetes Authors.
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

# Portions Copyright (c) 2025 NVIDIA CORPORATION. All rights reserved.
#
# Modified from the original to support gRPC transport.
# Origin: https://github.com/kubernetes/code-generator/blob/v0.34.1/kube_codegen.sh

set -o errexit
set -o nounset
set -o pipefail

KUBE_CODEGEN_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"

function kube::codegen::internal::get_goversion() {
    version=$(grep '^go ' "${KUBE_CODEGEN_ROOT}/go.mod" | awk '{print $2}')
    version=${version:-1.25}
    echo "$version"
}

function kube::codegen::internal::get_version() {
  local key="$1"
  local versions_file="${KUBE_CODEGEN_ROOT}/../.versions.yaml"
  if [[ -f "${versions_file}" ]]; then
    grep "${key}:" "${versions_file}" | sed -E 's/.*: *//' | tr -d " \"'" || true
  fi
}

if [[ -z "${KUBE_CODEGEN_TAG:-}" ]]; then
  if version=$(kube::codegen::internal::get_version "kubernetes_code_gen"); then
    KUBE_CODEGEN_TAG="${version}"
  fi
fi

# Callers which want a specific tag of the k8s.io/code-generator repo should
# set the KUBE_CODEGEN_TAG to the tag name, e.g. KUBE_CODEGEN_TAG="release-1.32"
# before sourcing this file.
CODEGEN_VERSION_SPEC="${KUBE_CODEGEN_TAG:+"@${KUBE_CODEGEN_TAG}"}"

if [[ -z "${PROTOC_GEN_GO_TAG:-}" ]]; then
  if version=$(kube::codegen::internal::get_version "protoc_gen_go"); then
    PROTOC_GEN_GO_TAG="${version}"
  fi
fi

# Callers which want a specific tag of the google.golang.org/protobuf repo should
# set the PROTOC_GEN_GO_TAG to the tag name, e.g. PROTOC_GEN_GO_TAG="v1.36.10"
# before sourcing this file.
PROTOC_GEN_GO_VERSION_SPEC="${PROTOC_GEN_GO_TAG:+"@${PROTOC_GEN_GO_TAG}"}"

if [[ -z "${PROTOC_GEN_GO_GRPC_TAG:-}" ]]; then
  if version=$(kube::codegen::internal::get_version "protoc_gen_go_grpc"); then
    PROTOC_GEN_GO_GRPC_TAG="${version}"
  fi
fi

# Callers which want a specific tag of the google.golang.org/grpc repo should
# set the PROTOC_GEN_GO_GRPC_TAG to the tag name, e.g. PROTOC_GEN_GO_GRPC_TAG="v1.5.1"
# before sourcing this file.
PROTOC_GEN_GO_GRPC_VERSION_SPEC="${PROTOC_GEN_GO_GRPC_TAG:+"@${PROTOC_GEN_GO_GRPC_TAG}"}"

if [[ -z "${GOVERTER_TAG:-}" ]]; then
  if version=$(kube::codegen::internal::get_version "goverter"); then
    GOVERTER_TAG="${version}"
  fi
fi

# Callers which want a specific tag of the x repo should
# set the GOVERTER_TAG to the tag name, e.g. GOVERTER_TAG="v1.9.2"
# before sourcing this file.
GOVERTER_VERSION_SPEC="${GOVERTER_TAG:+"@${GOVERTER_TAG}"}"

# Go installs in $GOBIN if defined, and $GOPATH/bin otherwise. We want to know
# which one it is, so we can use it later.
function get_gobin() {
    local from_env
    from_env="$(go env GOBIN)"
    if [[ -n "${from_env}" ]]; then
        echo "${from_env}"
    else
        echo "$(go env GOPATH)/bin"
    fi
}
GOBIN="$(get_gobin)"
export GOBIN

function kube::codegen::internal::findz() {
    # We use `find` rather than `git ls-files` because sometimes external
    # projects use this across repos.  This is an imperfect wrapper of find,
    # but good enough for this script.
    find "$@" -print0
}

function kube::codegen::internal::grep() {
    # We use `grep` rather than `git grep` because sometimes external projects
    # use this across repos.
    grep "$@" \
        --exclude-dir .git \
        --exclude-dir _output \
        --exclude-dir vendor
}

# Generate protobuf bindings
#
# USAGE: kube::codegen::gen_proto_bindings [FLAGS] <input-dir>
#
# <input-dir>
#   The root directory under which to search for Protobuf files to generate
#   bindings for. This must be a local path, not a Go package.
#
# FLAGS:
#
#   --output-dir <string = "gen/go">
#     The relative path under which to emit code.
#
#   --proto-root <string = "proto">
#     The relative path under which to search for Protobuf definitions.
function kube::codegen::gen_proto_bindings(){
  local in_dir=""
  local out_dir="gen/go"
  local proto_root="proto"
  local v="${KUBE_VERBOSE:-0}"

  while [ "$#" -gt 0 ]; do
    case "$1" in
        "--output-dir")
          out_dir="$2"
          shift 2
          ;;
        "--proto-root")
          proto_root="$2"
          shift 2
          ;;
      *)
          if [[ "$1" =~ ^-- ]]; then
              echo "unknown argument: $1" >&2
              return 1
          fi
          if [ -n "$in_dir" ]; then
              echo "too many arguments: $1 (already have $in_dir)" >&2
              return 1
          fi
          in_dir="$1"
          shift
          ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        UPSTREAM_BINS=(
            google.golang.org/protobuf/cmd/protoc-gen-go"${PROTOC_GEN_GO_VERSION_SPEC}"
            google.golang.org/grpc/cmd/protoc-gen-go-grpc"${PROTOC_GEN_GO_GRPC_VERSION_SPEC}"
        )
        echo "Installing upstream generators..."
        for bin in "${UPSTREAM_BINS[@]}"; do
          echo " - ${bin}"
          GO111MODULE=on go install "${bin}"
        done
    )

    # Go bindings
    #
    local input_versions=()
    while read -r dir; do
      local version="${dir#"${in_dir}/${proto_root}/"}"
      input_versions+=("${version}")
    done < <(
        ( kube::codegen::internal::findz \
            "${in_dir}/${proto_root}" \
            -type f \
            -name '*.proto' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_versions[@]}" != 0 ]; then
        echo "Generating Go protobuf bindings for ${#input_versions[@]} targets"

        for version in "${input_versions[@]}"; do
          if [ -d "${in_dir}/${out_dir}/${version}" ]; then
              ( kube::codegen::internal::findz \
                  "${in_dir}/${out_dir}/${version}" \
                  -maxdepth 1 \
                  -type f \
                  -name '*.pb.go' \
                  || true \
              ) | xargs -0 rm -f
          fi
        done

        (
          cd "${in_dir}/${proto_root}"
          for version in "${input_versions[@]}"; do
            mkdir -p "../${out_dir}/${version}"
            protoc \
              -I . \
              --plugin="protoc-gen-go=${GOBIN}/protoc-gen-go" \
              --plugin="protoc-gen-go-grpc=${GOBIN}/protoc-gen-go-grpc" \
              --go_out="../${out_dir}" \
              --go_opt=paths="source_relative" \
              --go-grpc_out="../${out_dir}" \
              --go-grpc_opt=paths="source_relative" \
              "${version}"/*.proto
          done
        )
    fi
}

# Generate tagged helper code: conversions, deepcopy, defaults and validations
#
# USAGE: kube::codegen::gen_helpers [FLAGS] <input-dir>
#
# <input-dir>
#   The root directory under which to search for Go files which request code to
#   be generated.  This must be a local path, not a Go package.
#
#   See note at the top about package structure below that.
#
# FLAGS:
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
#   --extra-peer-dir <string>
#     An optional list (this flag may be specified multiple times) of "extra"
#     directories to consider during conversion generation.
#
function kube::codegen::gen_helpers() {
    local in_dir=""
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local v="${KUBE_VERBOSE:-0}"
    local extra_peers=()

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            "--extra-peer-dir")
                extra_peers+=("$2")
                shift 2
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"
        UPSTREAM_BINS=(
            "k8s.io/code-generator/cmd/conversion-gen${CODEGEN_VERSION_SPEC}"
            "k8s.io/code-generator/cmd/deepcopy-gen${CODEGEN_VERSION_SPEC}"
            "k8s.io/code-generator/cmd/defaulter-gen${CODEGEN_VERSION_SPEC}"
            "k8s.io/code-generator/cmd/validation-gen${CODEGEN_VERSION_SPEC}"
        )
        echo "Installing upstream generators..."
        for bin in "${UPSTREAM_BINS[@]}"; do
          echo " - ${bin}"
        done
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install -a $(printf "%s " "${UPSTREAM_BINS[@]}")

        echo "Installing goverter..."
        rm -f "${GOBIN}/goverter"
        local tmp_dir
        tmp_dir=$(mktemp -d)
        trap 'rm -rf -- "$tmp_dir"' EXIT

        local goversion
        goversion=$(kube::codegen::internal::get_goversion)

        pushd "${tmp_dir}" > /dev/null
            go mod init build-goverter > /dev/null 2>&1
            go mod edit -go="${goversion}" > /dev/null 2>&1
            export GOTOOLCHAIN=auto
            go get "github.com/jmattheis/goverter${GOVERTER_VERSION_SPEC}" > /dev/null 2>&1
            go build -o "${GOBIN}/goverter" "github.com/jmattheis/goverter/cmd/goverter" > /dev/null
        popd > /dev/null
        echo " - github.com/jmattheis/goverter${GOVERTER_VERSION_SPEC}"
    )

    # Deepcopy
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:deepcopy-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating deepcopy code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.deepcopy.go \
            | xargs -0 rm -f

        "${GOBIN}/deepcopy-gen" \
            -v "${v}" \
            --output-file zz_generated.deepcopy.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi

    # Validations
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:validation-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating validation code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.validations.go \
            | xargs -0 rm -f

        "${GOBIN}/validation-gen" \
            -v "${v}" \
            --output-file zz_generated.validations.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi

    # Defaults
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:defaulter-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating defaulter code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.defaults.go \
            | xargs -0 rm -f

        "${GOBIN}/defaulter-gen" \
            -v "${v}" \
            --output-file zz_generated.defaults.go \
            --go-header-file "${boilerplate}" \
            "${input_pkgs[@]}"
    fi

    # Conversions
    #
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        input_pkgs+=("${pkg}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*+k8s:conversion-gen=' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_pkgs[@]}" != 0 ]; then
        echo "Generating conversion code for ${#input_pkgs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.conversion.go \
            | xargs -0 rm -f

        local extra_peer_args=()
        for arg in "${extra_peers[@]:+"${extra_peers[@]}"}"; do
            extra_peer_args+=("--extra-peer-dirs" "$arg")
        done
        "${GOBIN}/conversion-gen" \
            -v "${v}" \
            --output-file zz_generated.conversion.go \
            --go-header-file "${boilerplate}" \
            "${extra_peer_args[@]:+"${extra_peer_args[@]}"}" \
            "${input_pkgs[@]}"
    fi

    local input_dirs=()
    while read -r dir; do
        input_dirs+=("${dir}")
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^\s*//\s*goverter:converter' \
            -r "${in_dir}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#input_dirs[@]}" != 0 ]; then
        echo "Generating goverter conversion code for ${#input_dirs[@]} targets"

        kube::codegen::internal::findz \
            "${in_dir}" \
            -type f \
            -name zz_generated.goverter.go \
            | xargs -0 rm -f

        for dir in "${input_dirs[@]}"; do
          "${GOBIN}/goverter" \
              gen \
              "${dir}"
        done
    fi
}

# Generate client code
#
# USAGE: kube::codegen::gen_client [FLAGS] <input-dir>
#
# <input-dir>
#   The root package under which to search for Go files which request clients
#   to be generated. This must be a local path, not a Go package.
#
# FLAGS:
#   --one-input-api <string>
#     A specific API (a directory) under the input-dir for which to generate a
#     client.  If this is not set, clients for all APIs under the input-dir
#     will be generated (under the --output-pkg).
#
#   --output-dir <string>
#     The root directory under which to emit code.  Each aspect of client
#     generation will make one or more subdirectories.
#
#   --output-pkg <string>
#     The Go package path (import path) of the --output-dir.  Each aspect of
#     client generation will make one or more sub-packages.
#
#   --boilerplate <string = path_to_kube_codegen_boilerplate>
#     An optional override for the header file to insert into generated files.
#
#   --clientset-name <string = "clientset">
#     An optional override for the leaf name of the generated "clientset" directory.
#
#   --versioned-name <string = "versioned">
#     An optional override for the leaf name of the generated
#     "<clientset>/versioned" directory.
#
#   --with-watch
#     Enables generation of listers and informers for APIs which support WATCH.
#
#   --listers-name <string = "listers">
#     An optional override for the leaf name of the generated "listers" directory.
#
#   --informers-name <string = "informers">
#     An optional override for the leaf name of the generated "informers" directory.
#
#   --plural-exceptions <string = "">
#     An optional list of comma separated plural exception definitions in Type:PluralizedType form.
#
#   --proto-base <string>
#     The base Go import-path of the protobuf stubs.
#
function kube::codegen::gen_client() {
    local in_dir=""
    local one_input_api=""
    local out_dir=""
    local out_pkg=""
    local clientset_subdir="clientset"
    local clientset_versioned_name="versioned"
    local watchable="false"
    local listers_subdir="listers"
    local informers_subdir="informers"
    local boilerplate="${KUBE_CODEGEN_ROOT}/hack/boilerplate.go.txt"
    local plural_exceptions=""
    local v="${KUBE_VERBOSE:-0}"
    local proto_base=""

    while [ "$#" -gt 0 ]; do
        case "$1" in
            "--one-input-api")
                one_input_api="/$2"
                shift 2
                ;;
            "--output-dir")
                out_dir="$2"
                shift 2
                ;;
            "--output-pkg")
                out_pkg="$2"
                shift 2
                ;;
            "--boilerplate")
                boilerplate="$2"
                shift 2
                ;;
            "--clientset-name")
                clientset_subdir="$2"
                shift 2
                ;;
            "--versioned-name")
                clientset_versioned_name="$2"
                shift 2
                ;;
            "--with-watch")
                watchable="true"
                shift
                ;;
            "--listers-name")
                listers_subdir="$2"
                shift 2
                ;;
            "--informers-name")
                informers_subdir="$2"
                shift 2
                ;;
            "--plural-exceptions")
                plural_exceptions="$2"
                shift 2
                ;;
            "--proto-base")
                proto_base="$2"
                shift 2
                ;;
            *)
                if [[ "$1" =~ ^-- ]]; then
                    echo "unknown argument: $1" >&2
                    return 1
                fi
                if [ -n "$in_dir" ]; then
                    echo "too many arguments: $1 (already have $in_dir)" >&2
                    return 1
                fi
                in_dir="$1"
                shift
                ;;
        esac
    done

    if [ -z "${in_dir}" ]; then
        echo "input-dir argument is required" >&2
        return 1
    fi
    if [ -z "${out_dir}" ]; then
        echo "--output-dir is required" >&2
        return 1
    fi
    if [ -z "${out_pkg}" ]; then
        echo "--output-pkg is required" >&2
        return 1
    fi
    if [ -z "${proto_base}" ]; then
        echo "--proto-base is required for gRPC client generation" >&2
        return 1
    fi

    mkdir -p "${out_dir}"

    (
        # To support running this from anywhere, first cd into this directory,
        # and then install with forced module mode on and fully qualified name.
        cd "${KUBE_CODEGEN_ROOT}"

        UPSTREAM_BINS=(
            informer-gen"${CODEGEN_VERSION_SPEC}"
            lister-gen"${CODEGEN_VERSION_SPEC}"
        )
        echo "Installing upstream generators..."
        for bin in "${UPSTREAM_BINS[@]}"; do
          echo " - k8s.io/code-generator/cmd/${bin}"
        done
        # shellcheck disable=2046 # printf word-splitting is intentional
        GO111MODULE=on go install $(printf "k8s.io/code-generator/cmd/%s " "${UPSTREAM_BINS[@]}")

        echo "Installing local generators..."
        rm -f "${GOBIN}/client-gen"
        GO111MODULE=on go build -a -o "${GOBIN}/client-gen" ./cmd/client-gen
        echo " - github.com/nvidia/nvsentinel/code-generator/cmd/client-gen${CODEGEN_VERSION_SPEC}"
    )

    local group_versions=()
    local input_pkgs=()
    while read -r dir; do
        pkg="$(cd "${dir}" && GO111MODULE=on go list -find .)"
        leaf="$(basename "${dir}")"
        if grep -E -q '^v[0-9]+((alpha|beta)[0-9]+)?$' <<< "${leaf}"; then
            input_pkgs+=("${pkg}")

            dir2="$(dirname "${dir}")"
            leaf2="$(basename "${dir2}")"
            group_versions+=("${leaf2}/${leaf}")
        fi
    done < <(
        ( kube::codegen::internal::grep -l --null \
            -e '^[[:space:]]*//[[:space:]]*+genclient' \
            -r "${in_dir}${one_input_api}" \
            --include '*.go' \
            || true \
        ) | while read -r -d $'\0' F; do dirname "${F}"; done \
          | LC_ALL=C sort -u
    )

    if [ "${#group_versions[@]}" == 0 ]; then
        return 0
    fi

    echo "Generating client code for ${#group_versions[@]} targets"

    ( kube::codegen::internal::grep -l --null \
        -e '^// Code generated by client-gen. DO NOT EDIT.$' \
        -r "${out_dir}/${clientset_subdir}" \
        --include '*.go' \
        || true \
    ) | xargs -0 rm -f

    local inputs=()
    for arg in "${group_versions[@]}"; do
        inputs+=("--input" "$arg")
    done

    "${GOBIN}/client-gen" \
        -v "${v}" \
        --go-header-file "${boilerplate}" \
        --output-dir "${out_dir}/${clientset_subdir}" \
        --output-pkg "${out_pkg}/${clientset_subdir}" \
        --clientset-name "${clientset_versioned_name}" \
        --input-base "$(cd "${in_dir}" && pwd -P)" \
        --plural-exceptions "${plural_exceptions}" \
        --proto-base="${proto_base}" \
        "${inputs[@]}"

    if [ "${watchable}" == "true" ]; then
        echo "Generating lister code for ${#input_pkgs[@]} targets"

        ( kube::codegen::internal::grep -l --null \
            -e '^// Code generated by lister-gen. DO NOT EDIT.$' \
            -r "${out_dir}/${listers_subdir}" \
            --include '*.go' \
            || true \
        ) | xargs -0 rm -f

        "${GOBIN}/lister-gen" \
            -v "${v}" \
            --go-header-file "${boilerplate}" \
            --output-dir "${out_dir}/${listers_subdir}" \
            --output-pkg "${out_pkg}/${listers_subdir}" \
            --plural-exceptions "${plural_exceptions}" \
            "${input_pkgs[@]}"

        echo "Generating informer code for ${#input_pkgs[@]} targets"

        ( kube::codegen::internal::grep -l --null \
            -e '^// Code generated by informer-gen. DO NOT EDIT.$' \
            -r "${out_dir}/${informers_subdir}" \
            --include '*.go' \
            || true \
        ) | xargs -0 rm -f

        "${GOBIN}/informer-gen" \
            -v "${v}" \
            --go-header-file "${boilerplate}" \
            --output-dir "${out_dir}/${informers_subdir}" \
            --output-pkg "${out_pkg}/${informers_subdir}" \
            --versioned-clientset-package "${out_pkg}/${clientset_subdir}/${clientset_versioned_name}" \
            --listers-package "${out_pkg}/${listers_subdir}" \
            --plural-exceptions "${plural_exceptions}" \
            "${input_pkgs[@]}"
    fi
}
