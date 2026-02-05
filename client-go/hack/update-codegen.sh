#!/usr/bin/env bash

#  Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
CODEGEN_ROOT="${REPO_ROOT}/code-generator"

export KUBE_CODEGEN_ROOT="${CODEGEN_ROOT}"

source "${CODEGEN_ROOT}/kube_codegen.sh"

kube::codegen::gen_client \
  --proto-base "github.com/nvidia/nvsentinel/api/gen/go" \
  --output-dir "${REPO_ROOT}/client-go" \
  --output-pkg "github.com/nvidia/nvsentinel/client-go" \
  --boilerplate "${REPO_ROOT}/client-go/hack/boilerplate.go.txt" \
  --clientset-name "client" \
  --versioned-name "versioned" \
  --with-watch \
  --listers-name "listers" \
  --informers-name "informers" \
  "${REPO_ROOT}/api"
