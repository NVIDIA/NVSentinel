all: binary

binary:
	go build -o platform-connectors  main.go

.PHONY: build
build:
	@echo "Building platform-connectors..."
	go build ./...

# Docker configuration matching GitLab CI
NVCR_CONTAINER_REPO ?= nvcr.io
NGC_ORG ?= nv-ngc-devops
CI_COMMIT_REF_NAME ?= $(shell git rev-parse --abbrev-ref HEAD)
SAFE_REF_NAME ?= $(shell echo $(CI_COMMIT_REF_NAME) | sed 's/\//-/g')
SAFE_REF_NAME := $(if $(SAFE_REF_NAME),$(SAFE_REF_NAME),local)
BUILDX_BUILDER ?= nvsentinel-builder
PLATFORMS ?= linux/arm64,linux/amd64

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
	@echo "Building Docker image for platform-connectors (local development)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:platform-connectors \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:platform-connectors,mode=max \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-platform-connectors:$(SAFE_REF_NAME) \
		.

# Docker publish target (matching GitLab CI exactly)
.PHONY: docker-publish
docker-publish: setup-buildx
	@echo "Building and publishing Docker image for platform-connectors (production)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:platform-connectors \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:platform-connectors,mode=max \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-platform-connectors:$(SAFE_REF_NAME) \
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
	@echo "Cleaning platform-connectors..."
	go clean ./...
	rm -f coverage.txt coverage.xml report.xml code-quality-report.json platform-connectors

# Help target
.PHONY: help
help:
	@echo "Platform Connectors Makefile - Matching .gitlab-ci.yml exactly"
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  NVCR_CONTAINER_REPO=$(NVCR_CONTAINER_REPO)"
	@echo "  NGC_ORG=$(NGC_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Build binary (default)"
	@echo "  binary     - Build the main binary"
	@echo "  build      - Build the module"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build    - Build Docker image (local development)"
	@echo "  docker-publish  - Build and publish Docker image (CI/production with --push)"
	@echo "  setup-buildx    - Setup Docker buildx builder"
	@echo "  image           - Legacy target (calls docker-build)"
	@echo "  publish         - Legacy target (calls docker-publish)"
	@echo ""
	@echo "Utility targets:"
	@echo "  clean      - Clean build artifacts and reports"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Notes:"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Images tagged with dynamic ref name: $(SAFE_REF_NAME)"
	@echo "  - Build cache is used for faster builds"
	@echo "  - Matches .gitlab-ci.yml configuration exactly"

