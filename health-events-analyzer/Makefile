# Health Events Analyzer Makefile
# Individual module build and test targets (requires private repo access)

# Copyright (c) 2024, NVIDIA CORPORATION. All rights reserved.
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

# Health-events-analyzer specific settings
CLEAN_EXTRA_FILES := health-events-analyzer

# Docker configuration
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../common.mk

# =============================================================================
# LEGACY COMPATIBILITY
# =============================================================================

.PHONY: image publish
image: docker-build
publish: docker-publish

# =============================================================================
# MODULE HELP
# =============================================================================

help:
	@echo "Health Events Analyzer Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This module analyzes and processes health events from various sources."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (standardized default)"
	@echo "  lint-test  - Run full lint and test suite"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet, lint, test, coverage, build, binary"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build, docker-publish, image, publish"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Uses standardized repo-root Docker build context"
