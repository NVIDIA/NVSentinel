# NCCL Loopback Preflight Check Makefile
# Individual module build and test targets

# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

# Module type and capabilities
IS_GO_MODULE := 1
HAS_DOCKER := 1

# nccl-loopback specific settings
CLEAN_EXTRA_FILES := nccl-loopback-check

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../../make/common.mk
include ../../make/go.mk
include ../../make/docker.mk

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint-test ## Run lint-test (default target)

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "NCCL Loopback Preflight Check - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets:"
	@echo "  all            - Run lint-test (default)"
	@echo "  build          - Build Go binary"
	@echo "  test           - Run tests"
	@echo "  lint           - Run linter"
	@echo "  docker-build   - Build Docker image"
	@echo "  docker-publish - Push Docker image to registry"
	@echo "  clean          - Clean build artifacts"
