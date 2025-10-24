# make/go.mk - Go-specific build targets for nvsentinel modules
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
#
# This file provides standardized Go build, test, and lint targets.
# Include this file in Go module Makefiles.

# =============================================================================
# GO MODULE TARGETS
# =============================================================================

ifeq ($(IS_GO_MODULE),1)

# Main lint-test target (standardized across all modules)
.PHONY: lint-test
lint-test: ## Lint and test Go module (vet + golangci-lint + gotestsum)
	@echo "Linting and testing $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config $(GOLANGCI_CONFIG_PATH) $(LINT_EXTRA_FLAGS) && \
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) ./... -coverprofile=coverage.txt -covermode atomic -coverpkg=github.com/nvidia/nvsentinel/$(MODULE_NAME)/... && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# CI test target (alias for lint-test for consistency)
.PHONY: ci-test
ci-test: lint-test ## CI test target (alias for lint-test)

# Individual build and test targets
.PHONY: vet
vet: ## Run go vet
	@echo "Running go vet on $(MODULE_NAME)..."
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	@echo "Running golangci-lint on $(MODULE_NAME)..."
	$(GOLANGCI_LINT) run --config $(GOLANGCI_CONFIG_PATH) $(LINT_EXTRA_FLAGS)

.PHONY: test
test: ## Run tests with coverage
	@echo "Running tests on $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) ./... -coverprofile=coverage.txt -covermode atomic -coverpkg=github.com/nvidia/nvsentinel/$(MODULE_NAME)/...

.PHONY: coverage
coverage: test ## Generate coverage reports
	@echo "Generating coverage reports for $(MODULE_NAME)..."
	$(GO) tool cover -func coverage.txt
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: build
build: ## Build Go module
	@echo "Building $(MODULE_NAME)..."
	$(GO) build ./...

# Binary target (standardized but configurable)
.PHONY: binary
binary: ## Build binary executable
	@echo "Building $(MODULE_NAME) binary..."
	$(GO) build -o $(BINARY_TARGET) $(BINARY_SOURCE)

# Standardized clean target
.PHONY: clean
clean: ## Clean build artifacts and test outputs
	@echo "Cleaning $(MODULE_NAME)..."
	$(GO) clean ./...
	rm -f coverage.txt coverage.xml report.xml code-quality-report.json $(CLEAN_EXTRA_FILES)

endif
