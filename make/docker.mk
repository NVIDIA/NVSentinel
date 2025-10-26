# make/docker.mk - Docker build targets for nvsentinel modules
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
#
# This file provides standardized Docker build targets.
# Include this file in module Makefiles that build Docker images.

# =============================================================================
# DOCKER TARGETS (only if HAS_DOCKER is enabled)
# =============================================================================

ifeq ($(HAS_DOCKER),1)

# Setup buildx builder (standardized across all modules)
.PHONY: setup-buildx
setup-buildx: ## Setup Docker buildx builder
	@echo "Setting up Docker buildx builder for $(MODULE_NAME)..."
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		(docker context create $(BUILDX_BUILDER)-context || true && \
		 docker buildx create --name $(BUILDX_BUILDER) $(BUILDX_BUILDER)-context --driver docker-container --driver-opt network=host --buildkitd-flags '--allow-insecure-entitlement network.host' && \
		 docker buildx use $(BUILDX_BUILDER) && \
		 docker buildx inspect --bootstrap)

# Standardized Docker build (always from repo root for consistency)
.PHONY: docker-build
docker-build: setup-buildx ## Build Docker image (multi-platform with cache)
	@echo "Building Docker image for $(MODULE_NAME) (local development)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
	--network=host \
	$(CACHE_FROM_ARG) \
	$(CACHE_TO_ARG) \
	$(DOCKER_EXTRA_ARGS) \
	$(DOCKER_LOAD_ARG) \
	-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/$(MODULE_NAME):$(SAFE_REF_NAME) \
	-f $(MODULE_PATH)/Dockerfile \
	.

# Simplified docker target (local builds)
.PHONY: docker
docker: docker-build ## Build Docker image (alias for docker-build)

# Local-only Docker build (no remote cache, faster for local development)
.PHONY: docker-build-local
docker-build-local: setup-buildx ## Build Docker image without remote cache (faster local builds)
	@echo "Building Docker image for $(MODULE_NAME) (local, no remote cache)..."
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(DOCKER_EXTRA_ARGS) \
		$(DOCKER_LOAD_ARG) \
		-t $(MODULE_NAME):local \
		-f $(MODULE_PATH)/Dockerfile \
		.

# Standardized Docker publish
.PHONY: docker-publish
docker-publish: setup-buildx ## Build and publish Docker image to registry
	@echo "Building and publishing Docker image for $(MODULE_NAME) (production)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG) \
		$(CACHE_TO_ARG) \
		$(DOCKER_EXTRA_ARGS) \
		--push \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/$(MODULE_NAME):$(SAFE_REF_NAME) \
		-f $(MODULE_PATH)/Dockerfile \
		.

endif
