# CSP Health Monitor Makefile
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

# CSP-health-monitor specific settings
CLEAN_EXTRA_FILES := csp-health-monitor

# Docker configuration
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../../common.mk

# =============================================================================
# LEGACY COMPATIBILITY
# =============================================================================

# Legacy targets for backwards compatibility
.PHONY: image publish
image: docker-build
	@echo "Legacy 'image' target - use 'docker-build' for local development"

publish: docker-publish
	@echo "Legacy 'publish' target - use 'docker-publish' for CI/production"

# =============================================================================
# MODULE HELP
# =============================================================================

help:
	@echo "CSP Health Monitor Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This module handles CSP-specific health monitoring operations."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo "  DOCKER_MODULE_PATH=$(DOCKER_MODULE_PATH)"
	@echo "  NVCR_CONTAINER_REPO=$(NVCR_CONTAINER_REPO)"
	@echo "  NGC_ORG=$(NGC_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo "  HAS_DOCKER=$(HAS_DOCKER)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (standardized default)"
	@echo "  lint-test  - Run full lint and test suite (matches CI)"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet        - Run go vet"
	@echo "  lint       - Run golangci-lint"
	@echo "  test       - Run tests with coverage"
	@echo "  coverage   - Generate coverage reports"
	@echo "  build      - Build the module"
	@echo "  binary     - Build the main binary"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build    - Build Docker image (local development)"
	@echo "  docker-publish  - Build and publish Docker image (CI/production)"
	@echo "  setup-buildx    - Setup Docker buildx builder"
	@echo "  image           - Legacy target (calls docker-build)"
	@echo "  publish         - Legacy target (calls docker-publish)"
	@echo ""
	@echo "Utility targets:"
	@echo "  clean      - Clean build artifacts and reports"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Uses standardized repo-root Docker build context"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Build cache enabled for faster builds"
	@echo "  - Matches CI/CD pipeline configuration exactly"
