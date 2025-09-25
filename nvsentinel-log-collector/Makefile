# NVSentinel Log Collector Makefile
# Container-only module build targets

# Docker configuration matching GitLab CI
NVCR_CONTAINER_REPO ?= nvcr.io
NGC_ORG ?= nv-ngc-devops
CI_COMMIT_REF_NAME ?= $(shell git rev-parse --abbrev-ref HEAD)
SAFE_REF_NAME ?= $(shell echo $(CI_COMMIT_REF_NAME) | sed 's/\//-/g')
SAFE_REF_NAME := $(if $(SAFE_REF_NAME),$(SAFE_REF_NAME),local)
BUILDX_BUILDER ?= nvsentinel-builder
PLATFORMS ?= linux/arm64,linux/amd64

# Default target
.PHONY: all
all: lint

# Lint and test this module (matches parent Makefile expectations)
.PHONY: lint-test
lint-test: lint

# Lint shell scripts
.PHONY: lint
lint:
	@echo "Linting log collector shell scripts..."
	shellcheck entrypoint.sh

# Setup buildx builder (matching GitLab CI setup)
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
docker-build: setup-buildx
	@echo "Building Docker image for log-collector (local development)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:log-collector \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:log-collector,mode=max \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-log-collector:$(SAFE_REF_NAME) \
		.

# Docker publish target (matching GitLab CI exactly)
.PHONY: docker-publish
docker-publish: setup-buildx
	@echo "Building and publishing Docker image for log-collector (production)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:log-collector \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:log-collector,mode=max \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-log-collector:$(SAFE_REF_NAME) \
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
	@echo "NVSentinel Log Collector Makefile - Matching .gitlab-ci.yml exactly"
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  NVCR_CONTAINER_REPO=$(NVCR_CONTAINER_REPO)"
	@echo "  NGC_ORG=$(NGC_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo ""
	@echo "Main targets:"
	@echo "  all               - Run lint (default)"
	@echo "  lint-test         - Run shell script linting (matches CI)"
	@echo "  lint              - Run shellcheck locally"
	@echo "  lint-ci           - Run shellcheck using exact CI Docker environment"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build    - Build Docker image (local development)"
	@echo "  docker-publish  - Build and publish Docker image (CI/production with --push)"
	@echo "  setup-buildx    - Setup Docker buildx builder"
	@echo "  image           - Legacy target (calls docker-build)"
	@echo "  publish         - Legacy target (calls docker-publish)"
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
	@echo "  - Matches .gitlab-ci.yml configuration exactly"