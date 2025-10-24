# Data Models Makefile
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

# Data-models specific settings
LINT_EXTRA_FLAGS := --out-format code-climate:code-quality-report.json,colored-line-number
CLEAN_EXTRA_FILES :=

# Exclude protobuf files from test coverage
TEST_EXCLUDE_PACKAGES := pkg/protos

# Docker configuration
HAS_DOCKER := 0
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../make/common.mk

# =============================================================================
# MODULE-SPECIFIC TARGETS
# =============================================================================

# Generate Go protobuf files for data-models (shared across all Go modules)
.PHONY: protos-generate
protos-generate: protos-clean
	@echo "Generating Go protobuf files for data-models..."
	@echo "=== Tool Versions ==="
	@echo "Go: $$(go version)"
	@echo "protoc: $$(protoc --version)"
	@echo "protoc-gen-go: $$(protoc-gen-go --version)"
	@echo "protoc-gen-go-grpc: $$(protoc-gen-go-grpc --version)"
	@echo "========================"
	# Generate Go protobuf files (shared by all Go modules)
	protoc -I protobufs/ \
		--go_out=pkg/protos/ --go_opt=paths=source_relative \
		--go-grpc_out=pkg/protos/ --go-grpc_opt=paths=source_relative \
		protobufs/health_event.proto

# Clean generated Go protobuf files
.PHONY: protos-clean
protos-clean:
	@echo "Cleaning generated Go protobuf files in data-models..."
	@echo "Removing Go protobuf files (.pb.go)..."
	find pkg/protos/ -name "*.pb.go" -type f -delete 2>/dev/null || true

# Override test target to exclude protobuf packages
test:
	@echo "Running tests on $(MODULE_NAME)..."
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) $$(go list ./... | grep -v $(TEST_EXCLUDE_PACKAGES)) -coverprofile=coverage.txt -covermode atomic

# Override lint-test to use the custom test target
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

help:
	@echo "Data Models Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This module contains shared data models and protobuf definitions."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
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
	@echo ""
	@echo "Protobuf targets:"
	@echo "  protos-generate - Generate Go protobuf files from .proto sources"
	@echo "  protos-clean    - Remove generated Go protobuf files"
	@echo ""
	@echo "Utility targets:"
	@echo "  clean      - Clean build artifacts and reports"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Contains shared Go data models and protobuf definitions"
	@echo "  - Generates Go protobuf files (shared by all Go modules)"
	@echo "  - Python protobuf files are generated in gpu-health-monitor"
	@echo "  - Excludes protobuf packages from test coverage"
	@echo "  - No Docker builds (library module only)"
	@echo "  - Uses standardized nvsentinel build patterns"
