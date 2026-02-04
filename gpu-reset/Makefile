# GPU reset Makefile
# Individual module build and test targets (requires private repo access)
#
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
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

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION
# =============================================================================

HAS_DOCKER := 1

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../make/common.mk
include ../make/go.mk
include ../make/docker.mk

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint

.PHONY: lint
lint:
	@echo "Linting GPU reset shell scripts..."
	shellcheck gpu_reset.sh

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "GPU reset Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint"
	@echo "Docker targets: docker, docker-build, docker-publish"
