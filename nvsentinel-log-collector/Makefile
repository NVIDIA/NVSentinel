# NVSentinel Log Collector Makefile
# Container-only module build targets

# Docker configuration
NVCR_CONTAINER_REPO ?= nvcr.io
NGC_ORG ?= nv-ngc-devops
CI_COMMIT_REF_NAME ?= $(shell git rev-parse --abbrev-ref HEAD)
SAFE_REF_NAME ?= $(shell echo $(CI_COMMIT_REF_NAME) | sed 's/\//-/g')
SAFE_REF_NAME := $(if $(SAFE_REF_NAME),$(SAFE_REF_NAME),local)
BUILDX_BUILDER ?= nvsentinel-builder
PLATFORMS ?= linux/arm64,linux/amd64
DOCKER_LOAD_ARG ?= --load

# Cache configuration (can be disabled via environment variables)
DISABLE_REGISTRY_CACHE ?= false
ifeq ($(DISABLE_REGISTRY_CACHE),true)
CACHE_FROM_ARG_LOG :=
CACHE_TO_ARG_LOG :=
CACHE_FROM_ARG_CLEANUP :=
CACHE_TO_ARG_CLEANUP :=
else
CACHE_FROM_ARG_LOG := --cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:log-collector
CACHE_TO_ARG_LOG := --cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:log-collector,mode=max
CACHE_FROM_ARG_CLEANUP := --cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:file-server-cleanup
CACHE_TO_ARG_CLEANUP := --cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:file-server-cleanup,mode=max
endif

# Default target
.PHONY: all
all: lint

# Lint and test this module (matches parent Makefile expectations)
.PHONY: lint-test
lint-test: lint

# Lint shell scripts and Python code
.PHONY: lint
lint: lint-shell lint-python

# Lint only log-collector components (shell scripts)
.PHONY: lint-log-collector
lint-log-collector: lint-shell

# Lint only file-server-cleanup components (Python code)
.PHONY: lint-file-server-cleanup
lint-file-server-cleanup: lint-python

# Lint shell scripts
.PHONY: lint-shell
lint-shell:
	@echo "Linting log collector shell scripts..."
	shellcheck entrypoint.sh

# Lint Python code using Poetry and Black (file-server-cleanup package)
.PHONY: lint-python
lint-python:
	@echo "Linting file-server-cleanup Python code..."
	poetry config virtualenvs.in-project true && \
	poetry install && \
	poetry run black --check .

# Format Python code using Poetry and Black
.PHONY: format-python
format-python:
	@echo "Formatting file-server-cleanup Python code..."
	poetry config virtualenvs.in-project true && \
	poetry install && \
	poetry run black .

# Setup Python environment
.PHONY: setup-python
setup-python:
	@echo "Setting up Poetry environment..."
	poetry config virtualenvs.in-project true
	poetry install

# Setup buildx builder
.PHONY: setup-buildx
setup-buildx:
	@echo "Setting up Docker buildx builder..."
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		(docker context create $(BUILDX_BUILDER)-context || true && \
		 docker buildx create --name $(BUILDX_BUILDER) $(BUILDX_BUILDER)-context --driver docker-container --driver-opt network=host --buildkitd-flags '--allow-insecure-entitlement network.host' && \
		 docker buildx use $(BUILDX_BUILDER) && \
		 docker buildx inspect --bootstrap)

# Docker build target (for local development)
.PHONY: docker-build
docker-build: setup-buildx docker-build-log-collector docker-build-file-server-cleanup

# Build log-collector image (local development)
.PHONY: docker-build-log-collector
docker-build-log-collector: setup-buildx
	@echo "Building Docker image for log-collector (local development)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_LOG) \
		$(CACHE_TO_ARG_LOG) \
		$(DOCKER_LOAD_ARG) \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-log-collector:$(SAFE_REF_NAME) \
		.

# Build file-server-cleanup image (local development)
.PHONY: docker-build-file-server-cleanup
docker-build-file-server-cleanup: setup-buildx
	@echo "Building Docker image for file-server-cleanup (local development)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_CLEANUP) \
		$(CACHE_TO_ARG_CLEANUP) \
		$(DOCKER_LOAD_ARG) \
		-f Dockerfile.cleanup \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-file-server-cleanup:$(SAFE_REF_NAME) \
		.

# Docker publish target
.PHONY: docker-publish
docker-publish: setup-buildx docker-publish-log-collector docker-publish-file-server-cleanup

# Build and publish log-collector image
.PHONY: docker-publish-log-collector
docker-publish-log-collector: setup-buildx
	@echo "Building and publishing Docker image for log-collector (production)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_LOG) \
		$(CACHE_TO_ARG_LOG) \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-log-collector:$(SAFE_REF_NAME) \
		.

# Build and publish file-server-cleanup image
.PHONY: docker-publish-file-server-cleanup
docker-publish-file-server-cleanup: setup-buildx
	@echo "Building and publishing Docker image for file-server-cleanup (production)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG_CLEANUP) \
		$(CACHE_TO_ARG_CLEANUP) \
		--push \
		-f Dockerfile.cleanup \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-file-server-cleanup:$(SAFE_REF_NAME) \
		.

# Legacy targets for backwards compatibility
.PHONY: image
image: docker-build
	@echo "Legacy 'image' target - use 'docker-build' for local development or 'docker-publish' for CI/production"

.PHONY: publish
publish: docker-publish
	@echo "Legacy 'publish' target - use 'docker-publish' for CI/production"

# Clean target
.PHONY: clean
clean:
	@echo "Cleaning log-collector..."
	@echo "No build artifacts to clean for shell-based module"

# Help target
.PHONY: help
help:
	@echo "NVSentinel Log Collector Makefile - Standardized build system"
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  NVCR_CONTAINER_REPO=$(NVCR_CONTAINER_REPO)"
	@echo "  NGC_ORG=$(NGC_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo ""
	@echo "Main targets:"
	@echo "  all                        - Run lint (default)"
	@echo "  lint-test                  - Run all linting (matches CI)"
	@echo "  lint                       - Run all linting (shell + Python)"
	@echo "  lint-log-collector         - Run linting for log-collector only (shell scripts)"
	@echo "  lint-file-server-cleanup   - Run linting for file-server-cleanup only (Python)"
	@echo "  lint-shell                 - Run shellcheck on shell scripts"
	@echo "  lint-python                - Run Black check on Python code"
	@echo "  format-python              - Format Python code with Black"
	@echo "  setup-python               - Setup Poetry environment"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build                      - Build both images (local development)"
	@echo "  docker-build-log-collector        - Build log-collector image only (local)"
	@echo "  docker-build-file-server-cleanup  - Build file-server-cleanup image only (local)"
	@echo "  docker-publish                    - Build and publish both images (CI/production)"
	@echo "  docker-publish-log-collector      - Build and publish log-collector image only"
	@echo "  docker-publish-file-server-cleanup - Build and publish file-server-cleanup image only"
	@echo "  setup-buildx                      - Setup Docker buildx builder"
	@echo "  image                             - Legacy target (calls docker-build)"
	@echo "  publish                           - Legacy target (calls docker-publish)"
	@echo ""
	@echo "Utility targets:"
	@echo "  clean      - Clean build artifacts (no-op for shell module)"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Notes:"
	@echo "  - This is a container-only module using shell scripts"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Images tagged with dynamic ref name: $(SAFE_REF_NAME)"
	@echo "  - Build cache is used for faster builds"
	@echo "  - Multi-platform builds with build caching"
