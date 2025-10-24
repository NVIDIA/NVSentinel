# make/python.mk - Python-specific build targets for nvsentinel modules
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
#
# This file provides standardized Python build, test, and lint targets.
# Include this file in Python module Makefiles.

# =============================================================================
# PYTHON MODULE TARGETS
# =============================================================================

ifneq ($(IS_GO_MODULE),1)

# Python-specific test setup
PYTHON_TEST_SETUP ?= poetry config virtualenvs.in-project true && poetry install &&

# Main lint-test target (Python version)
.PHONY: lint-test
lint-test: ## Lint and test Python module (Black + pytest + coverage)
	@echo "Linting and testing $(MODULE_NAME) (Python module)..."
	$(PYTHON_TEST_SETUP) \
	poetry run black --check . && \
	poetry run coverage run --source=$(MODULE_NAME) -m pytest -vv --junitxml=report.xml && \
	(poetry run coverage xml || true) && \
	(poetry run coverage report || true)

# CI test target (alias for lint-test for consistency)
.PHONY: ci-test
ci-test: lint-test ## CI test target (alias for lint-test)

# Individual targets
.PHONY: vet
vet: ## Python modules don't use 'go vet' - no-op
	@echo "Python modules don't use 'go vet' - skipping"

.PHONY: lint
lint: ## Run Black formatter check
	@echo "Running Black formatter check on $(MODULE_NAME)..."
	poetry run black --check .

.PHONY: test
test: ## Run tests with coverage
	@echo "Running tests on $(MODULE_NAME)..."
	$(PYTHON_TEST_SETUP) \
	poetry run coverage run --source=$(MODULE_NAME) -m pytest -vv --junitxml=report.xml

.PHONY: coverage
coverage: test ## Generate coverage reports
	@echo "Generating coverage reports for $(MODULE_NAME)..."
	poetry run coverage report
	poetry run coverage xml || true

.PHONY: build
build: ## Build Python package with Poetry
	@echo "Building $(MODULE_NAME) (Python module)..."
	poetry build

.PHONY: binary
binary: ## Python modules don't build binaries - use 'build' target
	@echo "Python modules don't build binaries directly - use 'build' target"

.PHONY: clean
clean: ## Clean build artifacts and test outputs
	@echo "Cleaning $(MODULE_NAME)..."
	rm -f coverage.txt coverage.xml report.xml code-quality-report.json $(CLEAN_EXTRA_FILES)
	rm -rf dist/ build/ *.egg-info

endif
