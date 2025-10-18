# Node Drainer Module Makefile
# Individual module build and test targets (requires private repo access)

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

# Node-drainer-module specific settings
CLEAN_EXTRA_FILES := node-drainer-module
BINARY_SOURCE := ./cmd/nodedrainer.go

# Special kubebuilder setup for tests
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
# LEGACY COMPATIBILITY
# =============================================================================

.PHONY: image publish docker-build-with-cache
image: docker-build
publish: docker-publish

# Additional target for CI with registry cache
docker-build-with-cache: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME) (with registry cache)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG) \
		$(CACHE_TO_ARG) \
		$(DOCKER_EXTRA_ARGS) \
		$(DOCKER_LOAD_ARG) \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-$(MODULE_NAME):$(SAFE_REF_NAME) \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

# =============================================================================
# MODULE HELP
# =============================================================================

help:
	@echo "Node Drainer Module Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This module handles safe node draining and workload evacuation."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (standardized default)"
	@echo "  lint-test  - Run full lint and test suite"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet, lint, test, coverage, build, binary"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build    - Build Docker image (local development)"
	@echo "  docker-build-with-cache - Build with registry cache (CI)"
	@echo "  docker-publish  - Build and publish Docker image"
	@echo "  image, publish  - Legacy targets"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Binary built from cmd/nodedrainer.go"
	@echo "  - Uses standardized repo-root Docker build context"
