# GPU Health Monitor Makefile
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

# GPU-health-monitor specific settings (Python module)
CLEAN_EXTRA_FILES := gpu_health_monitor.egg-info

# Docker configuration - this module has dual DCGM support
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=

# Python-specific test setup
TEST_SETUP_COMMANDS := poetry config virtualenvs.in-project true && poetry install &&

# Python module - disable Go targets from common.mk
IS_GO_MODULE := 0

# Override lint-test for Python module (instead of Go)
LINT_EXTRA_FLAGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../../common.mk

# Cache configuration for specialized builds (respect DISABLE_REGISTRY_CACHE)
ifeq ($(DISABLE_REGISTRY_CACHE),true)
CACHE_FROM_ARG_DCGM3 :=
CACHE_TO_ARG_DCGM3 :=
CACHE_FROM_ARG_DCGM4 :=
CACHE_TO_ARG_DCGM4 :=
else
CACHE_FROM_ARG_DCGM3 := --cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-3
CACHE_TO_ARG_DCGM3 := --cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-3,mode=max
CACHE_FROM_ARG_DCGM4 := --cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-4
CACHE_TO_ARG_DCGM4 := --cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-4,mode=max
endif

# =============================================================================
# DEFAULT TARGET
# =============================================================================

# Default target
.PHONY: all clean help
all: lint-test

# =============================================================================
# PYTHON-SPECIFIC OVERRIDES
# =============================================================================

# Override Go-based targets with Python equivalents
lint-test:
	@echo "Linting and testing $(MODULE_NAME) (Python module)..."
	$(TEST_SETUP_COMMANDS) \
	poetry run black --check . && \
	poetry run coverage run --source=gpu_health_monitor -m pytest -vv --junitxml=report.xml && \
	(poetry run coverage xml || true) && \
	(poetry run coverage report || true)

vet:
	@echo "Python modules don't use 'go vet' - skipping"

lint:
	@echo "Running Black formatter check on $(MODULE_NAME)..."
	poetry run black --check .

test:
	@echo "Running tests on $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	poetry run coverage run --source=gpu_health_monitor -m pytest -vv --junitxml=report.xml

coverage: test
	@echo "Generating coverage reports for $(MODULE_NAME)..."
	poetry run coverage report
	poetry run coverage xml || true

build:
	@echo "Building $(MODULE_NAME) (Python module)..."
	poetry build

binary:
	@echo "Python modules don't build binaries directly - use 'build' target"

clean:
	@echo "Cleaning $(MODULE_NAME)..."
	rm -f report.xml coverage.xml .coverage
	rm -rf .pytest_cache
	rm -rf $(CLEAN_EXTRA_FILES)
	find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete

# =============================================================================
# PYTHON-SPECIFIC DOCKER OVERRIDES
# =============================================================================

# Override docker builds to support dual DCGM versions
docker-build: setup-buildx docker-build-dcgm3 docker-build-dcgm4
	@echo "Built both DCGM 3.x and 4.x images locally"

docker-build-dcgm3: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME) with DCGM 3.x (local development)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_DCGM3) \
		$(CACHE_TO_ARG_DCGM3) \
		$(DOCKER_EXTRA_ARGS) \
		$(DOCKER_LOAD_ARG) \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-$(MODULE_NAME):$(SAFE_REF_NAME)-dcgm-3.x \
		--build-arg DCGM_VERSION=3.3.7-1-ubuntu22.04 \
		--build-arg PYTHONPATH=/usr/local/dcgm/bindings/python3 \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

docker-build-dcgm4: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME) with DCGM 4.x (local development)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_DCGM4) \
		$(CACHE_TO_ARG_DCGM4) \
		$(DOCKER_EXTRA_ARGS) \
		$(DOCKER_LOAD_ARG) \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-$(MODULE_NAME):$(SAFE_REF_NAME)-dcgm-4.x \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

docker-publish: setup-buildx docker-publish-dcgm3 docker-publish-dcgm4
	@echo "Published both DCGM 3.x and 4.x images to registry"

docker-publish-dcgm3: setup-buildx
	@echo "Building and publishing Docker image for $(MODULE_NAME) with DCGM 3.x (production)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_DCGM3) \
		$(CACHE_TO_ARG_DCGM3) \
		$(DOCKER_EXTRA_ARGS) \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-$(MODULE_NAME):$(SAFE_REF_NAME)-dcgm-3.x \
		--build-arg DCGM_VERSION=3.3.7-1-ubuntu22.04 \
		--build-arg PYTHONPATH=/usr/local/dcgm/bindings/python3 \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

docker-publish-dcgm4: setup-buildx
	@echo "Building and publishing Docker image for $(MODULE_NAME) with DCGM 4.x (production)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_DCGM4) \
		$(CACHE_TO_ARG_DCGM4) \
		$(DOCKER_EXTRA_ARGS) \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-$(MODULE_NAME):$(SAFE_REF_NAME)-dcgm-4.x \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

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
# LEGACY COMPATIBILITY
# =============================================================================

.PHONY: image publish image-dcgm3 image-dcgm4 publish-dcgm3 publish-dcgm4
image: docker-build
	@echo "Legacy 'image' target - use 'docker-build' for local development"

publish: docker-publish
	@echo "Legacy 'publish' target - use 'docker-publish' for CI/production"

image-dcgm3: docker-build-dcgm3
	@echo "Built DCGM 3.x image locally (legacy target)"

image-dcgm4: docker-build-dcgm4
	@echo "Built DCGM 4.x image locally (legacy target)"

publish-dcgm3: docker-publish-dcgm3
	@echo "Published DCGM 3.x image to registry (legacy target)"

publish-dcgm4: docker-publish-dcgm4
	@echo "Published DCGM 4.x image to registry (legacy target)"

# =============================================================================
# MODULE HELP
# =============================================================================

help:
	@echo "GPU Health Monitor Makefile - Using nvsentinel common.mk standards (Python)"
	@echo ""
	@echo "This is a Python module that monitors GPU health using DCGM."
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
	@echo "Docker targets (dual DCGM support):"
	@echo "  docker-build         - Build both DCGM 3.x and 4.x images locally"
	@echo "  docker-build-dcgm3   - Build DCGM 3.x image locally"
	@echo "  docker-build-dcgm4   - Build DCGM 4.x image locally"
	@echo "  docker-publish       - Build and publish both images (CI/production)"
	@echo "  docker-publish-dcgm3 - Build and publish DCGM 3.x image"
	@echo "  docker-publish-dcgm4 - Build and publish DCGM 4.x image"
	@echo "  setup-buildx         - Setup Docker buildx builder"
	@echo ""
	@echo "Legacy targets:"
	@echo "  image                - Legacy target (calls docker-build)"
	@echo "  publish              - Legacy target (calls docker-publish)"
	@echo "  image-dcgm3          - Legacy: Build DCGM 3.x locally"
	@echo "  image-dcgm4          - Legacy: Build DCGM 4.x locally"
	@echo "  publish-dcgm3        - Legacy: Publish DCGM 3.x"
	@echo "  publish-dcgm4        - Legacy: Publish DCGM 4.x"
	@echo ""
	@echo "Development targets:"
	@echo "  shell      - Open Poetry shell"
	@echo "  deps       - Update dependencies"
	@echo "  deps-show  - Show dependencies"
	@echo "  clean      - Clean build artifacts and reports"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Python module using Poetry for dependency management"
	@echo "  - Supports both DCGM 3.x and 4.x versions"
	@echo "  - Uses standardized repo-root Docker build context"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Build cache enabled for faster builds"
	@echo "  - Matches CI/CD pipeline configuration exactly"
