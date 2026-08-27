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

format-protobuf-files:
	@echo "Formatting local protobuf files for $(MODULE_NAME)..."
	poetry run black $(PYTHON_PACKAGE_NAME)/protos/ || true

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
	@mkdir -p $(PYTHON_PACKAGE_NAME)/protos
	python3 -m grpc_tools.protoc -I ../../data-models/protobufs/ -I ../../data-models/vendor/ --python_out=$(PYTHON_PACKAGE_NAME)/protos --pyi_out=$(PYTHON_PACKAGE_NAME)/protos --grpc_python_out=$(PYTHON_PACKAGE_NAME)/protos ../../data-models/protobufs/health_event.proto
	# health_event.proto imports crd.proto (needed for CRD generation via protoc-gen-crd).
	# protoc propagates that import into the generated Python code, but the Go-style module path
	# (github.com/yandex/...) is unresolvable in Python. To fix this we:
	#   1. Generate crd_pb2.py from the vendored crd.proto using the vendor root as -I
	#      so the file descriptor name matches what health_event_pb2.py expects.
	#   2. Move crd_pb2.py from the nested output path into protos/ as a flat sibling.
	#   3. Rewrite the import in health_event_pb2.py to a relative import (from . import crd_pb2).
	python3 -m grpc_tools.protoc \
		-I ../../data-models/vendor/ \
		--python_out=$(PYTHON_PACKAGE_NAME)/protos \
		--pyi_out=$(PYTHON_PACKAGE_NAME)/protos \
		github.com/yandex/protoc-gen-crd/library/go/k8s/protoc_gen_crd/proto/crd.proto
	mv $(PYTHON_PACKAGE_NAME)/protos/github/com/yandex/protoc_gen_crd/library/go/k8s/protoc_gen_crd/proto/crd_pb2.py $(PYTHON_PACKAGE_NAME)/protos/crd_pb2.py
	mv $(PYTHON_PACKAGE_NAME)/protos/github/com/yandex/protoc_gen_crd/library/go/k8s/protoc_gen_crd/proto/crd_pb2.pyi $(PYTHON_PACKAGE_NAME)/protos/crd_pb2.pyi 2>/dev/null || true
	rm -rf $(PYTHON_PACKAGE_NAME)/protos/github
	# Fix relative imports in generated gRPC files
	@if command -v gsed >/dev/null 2>&1; then \
		gsed -i 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2_grpc.py; \
		gsed -i 's/^from github\.com\.yandex\.protoc_gen_crd\.library\.go\.k8s\.protoc_gen_crd\.proto import crd_pb2 as \(.*\)$$/from . import crd_pb2 as \1/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2.py; \
	elif [ "$$(uname)" = "Darwin" ]; then \
		sed -i '' 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2_grpc.py; \
		sed -i '' 's/^from github\.com\.yandex\.protoc_gen_crd\.library\.go\.k8s\.protoc_gen_crd\.proto import crd_pb2 as \(.*\)$$/from . import crd_pb2 as \1/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2.py; \
	else \
		sed -i 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2_grpc.py; \
		sed -i 's/^from github\.com\.yandex\.protoc_gen_crd\.library\.go\.k8s\.protoc_gen_crd\.proto import crd_pb2 as \(.*\)$$/from . import crd_pb2 as \1/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2.py; \
	fi
	@touch $(PYTHON_PACKAGE_NAME)/protos/__init__.py
	# Format generated protobuf files with Black
	@echo "Formatting generated protobuf files..."
	poetry run black $(PYTHON_PACKAGE_NAME)/protos/ || true

vendor-proto:
	$(MAKE) -C ../../data-models vendor-proto

.PHONY: protos-clean format-protobuf-files protos-generate vendor-proto
protos-clean: vendor-proto
	@echo "Cleaning generated protobuf files in $(MODULE_NAME)..."
	@echo "Removing Python protobuf files (*_pb2.py, *_pb2_grpc.py, *_pb2.pyi)..."
	find $(PYTHON_PACKAGE_NAME)/protos/ \( -name "*_pb2.py" -o -name "*_pb2_grpc.py" -o -name "*_pb2.pyi" \) -type f -delete 2>/dev/null || true
	rm -rf ./health-monitors/system-services-monitor/.venv || true

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
