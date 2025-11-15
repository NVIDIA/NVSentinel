# Simple Health Client Makefile
# Individual module build and test targets (requires private repo access)

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

# Simple-health-client specific settings
CLEAN_EXTRA_FILES := simple-health-client

# Go module configuration
IS_GO_MODULE := 1

# Docker configuration
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../../make/common.mk
include ../../make/go.mk

# =============================================================================
# LEGACY COMPATIBILITY
# =============================================================================

.PHONY: image publish
image: docker-build
publish: docker-publish

# =============================================================================
# MODULE HELP
# =============================================================================

help: ## Show help for available targets
	@echo "Simple Health Client Makefile - Using nvsentinel common.mk and go.mk standards"
	@echo ""
	@echo "Standard targets from go.mk:"
	@echo "  lint-test - Lint and test Go module (vet + golangci-lint + gotestsum)"
	@echo "  lint      - Run golangci-lint"
	@echo "  vet       - Run go vet"
	@echo "  test      - Run tests with coverage"
	@echo "  build     - Build Go binary"
	@echo "  clean     - Clean build artifacts"
	@echo ""
	@echo "Docker targets from common.mk:"
	@echo "  docker         - Build and publish Docker image"
	@echo "  docker-build   - Build Docker image"
	@echo "  docker-publish - Publish Docker image"
	@echo ""
	@echo "Legacy compatibility:"
	@echo "  image   - Alias for docker-build"
	@echo "  publish - Alias for docker-publish"
	@echo ""