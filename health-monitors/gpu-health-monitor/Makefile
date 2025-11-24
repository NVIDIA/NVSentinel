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

# Python module configuration
IS_GO_MODULE := 0
HAS_DOCKER := 1

# Python package name (with underscores, not hyphens)
PYTHON_PACKAGE_NAME := gpu_health_monitor

# GPU-health-monitor specific settings (Python module)
CLEAN_EXTRA_FILES := gpu_health_monitor.egg-info
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../../make/common.mk
include ../../make/python.mk
include ../../make/docker.mk

# Cache configuration for specialized builds (respect DISABLE_REGISTRY_CACHE)
ifeq ($(DISABLE_REGISTRY_CACHE),true)
CACHE_FROM_ARG_DCGM3 :=
CACHE_TO_ARG_DCGM3 :=
CACHE_FROM_ARG_DCGM4 :=
CACHE_TO_ARG_DCGM4 :=
else
CACHE_FROM_ARG_DCGM3 := --cache-from=type=registry,ref=$(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-3
CACHE_TO_ARG_DCGM3 := --cache-to=type=registry,ref=$(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-3,mode=max
CACHE_FROM_ARG_DCGM4 := --cache-from=type=registry,ref=$(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-4
CACHE_TO_ARG_DCGM4 := --cache-to=type=registry,ref=$(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel-buildcache:$(MODULE_NAME)-4,mode=max
endif

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint-test

# =============================================================================
# PYTHON-SPECIFIC OVERRIDES
# =============================================================================

# Detect OS for DCGM test compatibility
UNAME_S := $(shell uname -s)

# Override test target to skip DCGM tests on non-Linux systems
.PHONY: test
test: ## Run tests with coverage (skips DCGM tests on non-Linux)
ifneq ($(UNAME_S),Linux)
	@echo "⚠️  WARNING: DCGM tests require Linux with NVIDIA GPUs"
	@echo "⚠️  Skipping GPU health monitor tests on $(UNAME_S)"
	@echo "⚠️  To run full test suite, use a Linux system with NVIDIA GPUs"
	@mkdir -p $(CURDIR)
	@touch report.xml
	@echo '<?xml version="1.0" encoding="UTF-8"?><coverage version="0.0" timestamp="0"><packages/></coverage>' > coverage.xml
	@echo '<?xml version="1.0" encoding="utf-8"?><testsuites><testsuite name="pytest" tests="0" skipped="0" failures="0" errors="0" time="0" /></testsuites>' > report.xml
else
	@echo "Running tests on $(MODULE_NAME)..."
	$(PYTHON_TEST_SETUP) \
	poetry run coverage run --source=$(PYTHON_PACKAGE_NAME) -m pytest -vv --junitxml=report.xml
endif

# Override coverage target to skip on non-Linux systems
.PHONY: coverage
coverage: test ## Generate coverage reports (skips on non-Linux)
ifneq ($(UNAME_S),Linux)
	@echo "⚠️  Skipping coverage report generation on $(UNAME_S)"
	@echo "✓ Empty coverage files created for CI compatibility"
else
	@echo "Generating coverage reports for $(MODULE_NAME)..."
	poetry run coverage report
	poetry run coverage xml || true
endif

# Override lint-test to use the modified test and coverage targets
.PHONY: lint-test
lint-test: setup lint test coverage ## Run linting and tests (full pipeline)
ifneq ($(UNAME_S),Linux)
	@echo "✓ Linting passed (tests skipped on non-Linux system)"
else
	@echo "✓ All checks passed"
endif

# Python lint-test and other targets are provided by make/python.mk
# but we can override them here if needed
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
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-3.x \
		--build-arg DCGM_VERSION=3.3.7-1-ubuntu22.04 \
		--build-arg PYTHONPATH=/usr/local/dcgm/bindings/python3 \
		-f $(MODULE_PATH)/Dockerfile \
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
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-4.x \
		-f $(MODULE_PATH)/Dockerfile \
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
		$(if $(DOCKER_METADATA_FILE),--metadata-file $(DOCKER_METADATA_FILE)) \
		--push \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-3.x \
		--build-arg DCGM_VERSION=3.3.7-1-ubuntu22.04 \
		--build-arg PYTHONPATH=/usr/local/dcgm/bindings/python3 \
		-f $(MODULE_PATH)/Dockerfile \
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
		$(if $(DOCKER_METADATA_FILE),--metadata-file $(DOCKER_METADATA_FILE)) \
		--push \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-4.x \
		-f $(MODULE_PATH)/Dockerfile \
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

format-protobuf-files:
	@echo "Formatting local protobuf files for $(MODULE_NAME)..."
	poetry run black gpu_health_monitor/protos/ || true

protos-generate: protos-clean
	@echo "Generating protobuf files for $(MODULE_NAME)..."
	@echo "=== Tool Versions ==="
	@echo "protoc: $$(protoc --version)"
	@if command -v poetry >/dev/null 2>&1; then \
		grpcio_tools_version=$$(poetry run python -c "import importlib.metadata; print('grpcio-tools', importlib.metadata.version('grpcio-tools'))" 2>/dev/null || echo "grpcio-tools: not installed"); \
		echo "$$grpcio_tools_version"; \
		black_version=$$(poetry run black --version 2>/dev/null | head -1 || echo "black: not installed"); \
		echo "$$black_version"; \
	else \
		echo "grpcio-tools: Poetry not available"; \
		echo "black: Poetry not available"; \
	fi
	@echo "========================"
	# Generate Python protobuf files
	@echo "Generating Python protobuf files for $(MODULE_NAME)..."
	@mkdir -p gpu_health_monitor/protos
	python3 -m grpc_tools.protoc -I ../../data-models/protobufs/ --python_out=gpu_health_monitor/protos --pyi_out=gpu_health_monitor/protos --grpc_python_out=gpu_health_monitor/protos ../../data-models/protobufs/health_event.proto
	# Fix relative imports in generated gRPC files
	@if command -v gsed >/dev/null 2>&1; then \
		gsed -i 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' gpu_health_monitor/protos/health_event_pb2_grpc.py; \
	elif [ "$$(uname)" = "Darwin" ]; then \
		sed -i '' 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' gpu_health_monitor/protos/health_event_pb2_grpc.py; \
	else \
		sed -i 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' gpu_health_monitor/protos/health_event_pb2_grpc.py; \
	fi
	@touch gpu_health_monitor/protos/__init__.py
	# Format generated protobuf files with Black
	@echo "Formatting generated protobuf files..."
	poetry run black gpu_health_monitor/protos/ || true

# Clean generated protobuf files
.PHONY: protos-clean
protos-clean:
	@echo "Cleaning generated protobuf files in $(MODULE_NAME)..."
	@echo "Removing Python protobuf files (*_pb2.py, *_pb2_grpc.py, *_pb2.pyi)..."
	find gpu_health_monitor/protos/ \( -name "*_pb2.py" -o -name "*_pb2_grpc.py" -o -name "*_pb2.pyi" \) -type f -delete 2>/dev/null || true
	rm -rf ./health-monitors/gpu-health-monitor/.venv || true

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
	@echo "Protobuf targets:"
	@echo "  protos-generate       - Generate Python protobuf files from .proto sources"
	@echo "  format-protobuf-files - Format generated protobuf files with Black"
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
