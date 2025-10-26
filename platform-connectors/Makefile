# Platform Connectors Makefile
# Individual module build and test targets

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

IS_GO_MODULE := 1
IS_KO_MODULE := 1

# Platform-connectors specific settings
LINT_EXTRA_FLAGS := --out-format code-climate:code-quality-report.json,colored-line-number
CLEAN_EXTRA_FILES := platform-connectors

# Exclude protobuf files from test coverage
TEST_EXCLUDE_PACKAGES := -e pkg/protos

# Special kubebuilder setup for envtest-based tests
TEST_SETUP_COMMANDS := export PATH="$$GOPATH/bin:$$PATH" && \
                      go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest && \
                      export KUBEBUILDER_ASSETS=$$(setup-envtest use -p path 1.30.0) &&

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../make/common.mk
include ../make/go.mk

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint-test

# =============================================================================
# MODULE-SPECIFIC OVERRIDES
# =============================================================================

# Override test target to exclude protobuf packages and set up envtest
.PHONY: test
test:
	@echo "Running tests on $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) $$(go list ./... | grep -v $(TEST_EXCLUDE_PACKAGES)) -coverprofile=coverage.txt -covermode atomic

# Override lint-test to use the custom test target
.PHONY: lint-test
lint-test:
	@echo "Linting and testing $(MODULE_NAME)..."
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config $(GOLANGCI_CONFIG_PATH) $(LINT_EXTRA_FLAGS) && \
	$(MAKE) test && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "Platform Connectors Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "This module handles platform-specific connectors for health metrics."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo "  IS_GO_MODULE=1 (Go module with kubebuilder/envtest)"
	@echo "  IS_KO_MODULE=1 (ko container builds enabled)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (default)"
	@echo "  lint-test  - Run full lint and test suite"
	@echo "  ci-test    - Run CI test suite (alias for lint-test)"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet        - Run go vet"
	@echo "  lint       - Run golangci-lint with code-climate output"
	@echo "  test       - Run tests with coverage (excludes protobuf packages)"
	@echo "  coverage   - Generate coverage reports"
	@echo "  build      - Build the module"
	@echo "  binary     - Build the main binary"
	@echo "  clean      - Clean build artifacts"
	@echo "  ko-build   - Build container image using ko (local)"
	@echo "  ko-publish - Build and publish container image using ko"
	@echo ""
	@echo "Build notes:"
	@echo "  - Container images are built using ko"
	@echo "  - Use 'make ko-build' for local builds (KO_DOCKER_REPO=ko.local by default)"
	@echo "  - Use 'make ko-publish' to build and push (set KO_DOCKER_REPO and VERSION)"
	@echo "  - For Tilt development, see Tiltfile"
	@echo "  - Excludes protobuf packages from test coverage"
	@echo "  - Uses code-climate format for lint output"
	@echo "  - Runs tests with -short flag for faster execution"

