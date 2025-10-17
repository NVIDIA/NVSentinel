# Platform Connectors Makefile
# Individual module build and test targets

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

# Platform-connectors specific settings
LINT_EXTRA_FLAGS := --out-format code-climate:code-quality-report.json,colored-line-number
CLEAN_EXTRA_FILES := platform-connectors

# Exclude protobuf files from test coverage
TEST_EXCLUDE_PACKAGES := -e pkg/protos

# Special kubebuilder setup for envtest-based tests
TEST_SETUP_COMMANDS := export PATH="$$GOPATH/bin:$$PATH" && \
                      go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest && \
                      export KUBEBUILDER_ASSETS=$$(setup-envtest use -p path 1.30.0) &&

# Docker configuration
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=


# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../common.mk

# =============================================================================
# MODULE-SPECIFIC OVERRIDES
# =============================================================================

# Override test target to exclude protobuf packages and set up envtest
test:
	@echo "Running tests on $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) $$(go list ./... | grep -v $(TEST_EXCLUDE_PACKAGES)) -coverprofile=coverage.txt -covermode atomic

# Override lint-test to use the custom test target
lint-test:
	@echo "Linting and testing $(MODULE_NAME)..."
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml $(LINT_EXTRA_FLAGS) && \
	$(MAKE) test && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

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
	@echo "Platform Connectors Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This module handles platform-specific connectors for health metrics."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
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
	@echo "  lint       - Run golangci-lint with code-climate output"
	@echo "  test       - Run tests with coverage (excludes protobuf packages)"
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
	@echo "  - Excludes protobuf packages from test coverage"
	@echo "  - Uses code-climate format for lint output"
	@echo "  - Runs tests with -short flag for faster execution"
	@echo "  - Uses standardized repo-root Docker build context"
