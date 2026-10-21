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

FROM nvcr.io/nvidia/cuda:13.4.3-runtime-ubuntu24.04@sha256:f58bd7a0701fae3a2160f22e1d451b77b349bd9ef83dad76f5529aa413c149a8

ARG DEBIAN_FRONTEND=noninteractive

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update \
  && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
  && rm -rf /var/lib/apt/lists/*

COPY gpu-reset/gpu_reset.sh /usr/local/bin/gpu_reset.sh

RUN chmod +x /usr/local/bin/gpu_reset.sh

ENTRYPOINT ["/usr/local/bin/gpu_reset.sh"]
