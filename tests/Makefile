# Tests Makefile
# End-to-end test targets

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

# E2E Tests module configuration
IS_GO_MODULE := 1
TEST_EXTRA_FLAGS := -timeout 45m -count 1 -failfast

# Database-aware test tags:
# - USE_POSTGRESQL=1: excludes mongodb-only tests (e.g., health_events_analyzer_test.go)
# - Default (MongoDB): includes all tests
ifeq ($(USE_POSTGRESQL),1)
    TEST_TAGS ?= amd64_group,arm64_group
else
    TEST_TAGS ?= amd64_group,arm64_group,mongodb
endif

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../make/common.mk
include ../make/go.mk

# =============================================================================
# E2E-SPECIFIC TARGETS
# =============================================================================

# Default target
.PHONY: all
all: test

.PHONY: lint-test
lint-test: lint ## Run linting only (override go.mk comprehensive version)
	@echo "Linting complete for $(MODULE_NAME)"

# Run end-to-end tests in CI
.PHONY: test-ci
test-ci:
	nohup kubectl port-forward -n monitoring svc/prometheus-operator-kube-p-prometheus 9090:9090 > /dev/null 2>&1 &
	nohup kubectl port-forward -n nvsentinel svc/simple-health-client 8080:8080 > /dev/null 2>&1 &
	kubectl get po -Ao wide
	kubectl get no -o wide
	@echo "Restarting fault-quarantine deployment..."
	kubectl rollout restart deploy -l app.kubernetes.io/name=fault-quarantine -n nvsentinel
	@echo "Waiting for deployment rollout to complete..."
	kubectl rollout status deployment/fault-quarantine -n nvsentinel --timeout=10m
	$(MAKE) test

# Run end-to-end tests (override standard test to use gotestsum with custom format)
.PHONY: test
test: ## Run end-to-end tests (USE_POSTGRESQL=1 excludes MongoDB-only tests)
	@echo "Running end-to-end tests on $(MODULE_NAME) with tags: $(TEST_TAGS)..."
	$(GOTESTSUM) --format standard-verbose -- -tags=$(TEST_TAGS) $(TEST_EXTRA_FLAGS) ./... -coverprofile=coverage.txt -covermode atomic -coverpkg=github.com/nvidia/nvsentinel/$(MODULE_NAME)/...


# =============================================================================
# MODULE HELP
# =============================================================================

help: ## Show help for available targets
	@echo "Tests Makefile - E2E test execution with standard linting"
	@echo ""
	@echo "Standard targets from go.mk:"
	@echo "  lint-test - Lint and test Go module (vet + golangci-lint + gotestsum)"
	@echo "  lint      - Run golangci-lint"
	@echo "  vet       - Run go vet"
	@echo "  test      - Run end-to-end tests with coverage"
	@echo "  clean     - Clean build artifacts"
	@echo ""
	@echo "E2E-specific targets:"
	@echo "  all     - Run test (default)"
	@echo "  test-ci - Run end-to-end tests in CI environment"
	@echo "  help    - Show this help message"
	@echo ""
	@echo "Database-specific behavior:"
	@echo "  USE_POSTGRESQL=1 make test  - Excludes MongoDB-only tests (health_events_analyzer)"
	@echo "  make test                   - Includes all tests (default, for MongoDB)"
