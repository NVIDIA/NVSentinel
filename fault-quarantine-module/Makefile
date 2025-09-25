# Fault Quarantine Module Makefile
# Individual module build and test targets (requires private repo access)

# Go binary and tools
GO := go
GOLANGCI_LINT := golangci-lint
GOTESTSUM := gotestsum
GOCOVER_COBERTURA := gocover-cobertura

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
all: lint-test

# Lint and test this module (matches parent Makefile expectations)
.PHONY: lint-test
lint-test:
	@echo "Linting and testing fault-quarantine-module (with private repos)..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# Individual steps
.PHONY: vet
vet:
	@echo "Running go vet..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	$(GO) vet ./...

.PHONY: lint
lint:
	@echo "Running golangci-lint..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml

.PHONY: test
test:
	@echo "Running tests..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic

.PHONY: coverage
coverage: test
	@echo "Generating coverage reports..."
	$(GO) tool cover -func coverage.txt
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# Build targets
.PHONY: build
build:
	@echo "Building fault-quarantine-module..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	$(GO) build ./...

.PHONY: binary
binary:
	@echo "Building fault-quarantine-module binary..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	$(GO) build -o fault-quarantine-module .

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
	@echo "Building Docker image for fault-quarantine-module (local development)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:fault-quarantine-module \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:fault-quarantine-module,mode=max \
		--build-arg GITLAB_HOST=gitlab-master.nvidia.com \
		--build-arg GITLAB_LOGIN=gitlab-ci-token \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-fault-quarantine-module:$(SAFE_REF_NAME) \
		.

# Docker publish target (matching GitLab CI exactly)
.PHONY: docker-publish
docker-publish: setup-buildx
	@echo "Building and publishing Docker image for fault-quarantine-module (production)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:fault-quarantine-module \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:fault-quarantine-module,mode=max \
		--build-arg GITLAB_HOST=gitlab-master.nvidia.com \
		--build-arg GITLAB_LOGIN=gitlab-ci-token \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-fault-quarantine-module:$(SAFE_REF_NAME) \
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
	@echo "Cleaning fault-quarantine-module..."
	$(GO) clean ./...
	rm -f coverage.txt coverage.xml report.xml fault-quarantine-module

# Help target
.PHONY: help
help:
	@echo "Fault Quarantine Module Makefile - Matching .gitlab-ci.yml exactly"
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  NVCR_CONTAINER_REPO=$(NVCR_CONTAINER_REPO)"
	@echo "  NGC_ORG=$(NGC_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (default)"
	@echo "  lint-test  - Run full lint and test suite"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet        - Run go vet"
	@echo "  lint       - Run golangci-lint"
	@echo "  test       - Run tests with coverage"
	@echo "  coverage   - Generate coverage reports"
	@echo "  build      - Build the module"
	@echo "  binary     - Build the main binary"
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
	@echo "  - GOPRIVATE is automatically set for gitlab-master.nvidia.com/dgxcloud/mk8s/*"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Images tagged with dynamic ref name: $(SAFE_REF_NAME)"
	@echo "  - Build cache is used for faster builds"
	@echo "  - Special lint configuration:"
	@echo "  - Matches .gitlab-ci.yml configuration exactly"