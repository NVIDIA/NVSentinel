# System Services Monitor Makefile
# Individual module build and test targets (Python module)

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

# Python module configuration
IS_GO_MODULE := 0
HAS_DOCKER := 1

# Python package name (with underscores, not hyphens)
PYTHON_PACKAGE_NAME := system_services_monitor
# Coverage in make/python.mk uses --source=$(MODULE_NAME); common.mk defaults
# MODULE_NAME to the (hyphenated) directory basename, which does not match the
# importable package, yielding 0% coverage. Pin it to the package name.
MODULE_NAME := $(PYTHON_PACKAGE_NAME)

# system-services-monitor specific settings (Python module)
CLEAN_EXTRA_FILES := system_services_monitor.egg-info
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../../make/common.mk
include ../../make/python.mk
include ../../make/docker.mk

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint-test

# =============================================================================
# PYTHON-SPECIFIC DEVELOPMENT TARGETS
# =============================================================================

.PHONY: setup shell deps deps-show format
setup:
	@echo "Setting up Poetry environment for $(MODULE_NAME)..."
	poetry config virtualenvs.in-project true
	poetry install

shell:
	@echo "Opening Poetry shell for $(MODULE_NAME)..."
	poetry shell

deps:
	@echo "Updating dependencies for $(MODULE_NAME)..."
	poetry update

deps-show:
	@echo "Showing dependencies for $(MODULE_NAME)..."
	poetry show

format:
	@echo "Running Black formatter on $(MODULE_NAME)..."
	poetry run black .

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "System Services Monitor Makefile - Using nvsentinel make/*.mk standards (Python)"
	@echo ""
	@echo "This is a Python module that monitors non-DCGM service health on GPU nodes."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo "  CONTAINER_REGISTRY=$(CONTAINER_REGISTRY)"
	@echo "  CONTAINER_ORG=$(CONTAINER_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo "  HAS_DOCKER=$(HAS_DOCKER)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (standardized default)"
	@echo "  lint-test  - Run full lint and test suite (Python with Poetry)"
	@echo ""
	@echo "Individual targets:"
	@echo "  setup      - Set up Poetry environment"
	@echo "  lint       - Run Black formatter check"
	@echo "  format     - Run Black formatter"
	@echo "  test       - Run tests with coverage"
	@echo "  coverage   - Generate coverage reports"
	@echo "  build      - Build Python package"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build   - Build Docker image (local)"
	@echo "  docker-publish - Build and publish Docker image (CI/production)"
	@echo "  setup-buildx   - Setup Docker buildx builder"
	@echo ""
	@echo "Development targets:"
	@echo "  shell      - Open Poetry shell"
	@echo "  deps       - Update dependencies"
	@echo "  deps-show  - Show dependencies"
	@echo "  clean      - Clean build artifacts and reports"
	@echo "  help       - Show this help message"
