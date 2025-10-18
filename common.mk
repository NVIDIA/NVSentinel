# common.mk - Shared Makefile definitions for all nvsentinel Go modules
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
#
# This file provides standardized build, test, and Docker patterns for all Go modules
# in the nvsentinel project. Include this file in individual module Makefiles to
# ensure consistency across the project.
#
# Usage in module Makefile:
#   include ../common.mk
#   # Set module-specific configurations before including
#   # Override specific targets after including if needed

# =============================================================================
# STANDARDIZED VARIABLES
# =============================================================================

# Go binary and tools (standardized versions)
GO := go
GOLANGCI_LINT := golangci-lint
GOTESTSUM := gotestsum
GOCOVER_COBERTURA := gocover-cobertura

# Docker configuration (standardized across all modules)
NVCR_CONTAINER_REPO ?= nvcr.io
NGC_ORG ?= nv-ngc-devops
CI_COMMIT_REF_NAME ?= $(shell git rev-parse --abbrev-ref HEAD)
SAFE_REF_NAME ?= $(shell echo $(CI_COMMIT_REF_NAME) | sed 's/\//-/g')
SAFE_REF_NAME := $(if $(SAFE_REF_NAME),$(SAFE_REF_NAME),local)
BUILDX_BUILDER ?= nvsentinel-builder
PLATFORMS ?= linux/arm64,linux/amd64

# Auto-detect current module name from directory
MODULE_NAME := $(shell basename $(CURDIR))

# Cache configuration (can be disabled via environment variables)
DISABLE_REGISTRY_CACHE ?= false
ifeq ($(DISABLE_REGISTRY_CACHE),true)
CACHE_FROM_ARG :=
CACHE_TO_ARG :=
else
CACHE_FROM_ARG := --cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:$(MODULE_NAME)
CACHE_TO_ARG := --cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:$(MODULE_NAME),mode=max
endif

# Repository root path calculation (works from any subdirectory depth)
REPO_ROOT := $(shell git rev-parse --show-toplevel)

# Auto-detect Docker path relative to repo root (handles nested modules)
DOCKER_MODULE_PATH := $(subst $(REPO_ROOT)/,,$(CURDIR))

# Auto-detect golangci-lint config path (handles nested modules)
GOLANGCI_CONFIG_PATH := $(REPO_ROOT)/.golangci.yml

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION (set by individual Makefiles)
# =============================================================================

# No special environment setup needed - simplified build process

# Configurable lint flags (defaults to code-climate output)
LINT_EXTRA_FLAGS ?= --out-format code-climate:code-quality-report.json,colored-line-number

# Configurable test flags (e.g., TEST_EXTRA_FLAGS := -short)
TEST_EXTRA_FLAGS ?=

# Binary configuration (defaults to module name and current directory)
BINARY_TARGET ?= $(MODULE_NAME)
BINARY_SOURCE ?= .

# Docker configuration (can be overridden per module)
DOCKER_EXTRA_ARGS ?=
DOCKER_LOAD_ARG ?= --load
HAS_DOCKER ?= 1

# Module type configuration (Go=1, Python=0 - Python modules override targets)
IS_GO_MODULE ?= 1

# Additional clean files (module-specific artifacts)
CLEAN_EXTRA_FILES ?=

# Test setup commands (e.g., for kubebuilder)
TEST_SETUP_COMMANDS ?=

# =============================================================================
# PHONY DECLARATIONS
# =============================================================================

.PHONY: all lint-test vet lint test coverage build binary clean help
.PHONY: setup-buildx docker-build docker-build-local docker-publish image publish

# =============================================================================
# STANDARDIZED TARGETS
# =============================================================================

# Default target (standardized to lint-test for consistency)
all: lint-test

# =============================================================================
# GO MODULE TARGETS (conditional based on IS_GO_MODULE)
# =============================================================================

ifeq ($(IS_GO_MODULE),1)

# Main lint-test target (standardized across all modules)
lint-test:
	@echo "Linting and testing $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config $(GOLANGCI_CONFIG_PATH) $(LINT_EXTRA_FLAGS) && \
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) ./... -coverprofile=coverage.txt -covermode atomic -coverpkg=./... && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# Individual build and test targets
vet:
	@echo "Running go vet on $(MODULE_NAME)..."
	$(GO) vet ./...

lint:
	@echo "Running golangci-lint on $(MODULE_NAME)..."
	$(GOLANGCI_LINT) run --config $(GOLANGCI_CONFIG_PATH) $(LINT_EXTRA_FLAGS)

test:
	@echo "Running tests on $(MODULE_NAME)..."
	$(TEST_SETUP_COMMANDS) \
	$(GOTESTSUM) --junitfile report.xml -- -race $(TEST_EXTRA_FLAGS) ./... -coverprofile=coverage.txt -covermode atomic -coverpkg=./...

coverage: test
	@echo "Generating coverage reports for $(MODULE_NAME)..."
	$(GO) tool cover -func coverage.txt
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

build:
	@echo "Building $(MODULE_NAME)..."
	$(GO) build ./...

# Binary target (standardized but configurable)
binary:
	@echo "Building $(MODULE_NAME) binary..."
	$(GO) build -o $(BINARY_TARGET) $(BINARY_SOURCE)

# Standardized clean target
clean:
	@echo "Cleaning $(MODULE_NAME)..."
	$(GO) clean ./...
	rm -f coverage.txt coverage.xml report.xml code-quality-report.json $(CLEAN_EXTRA_FILES)

else

# For non-Go modules (like Python), targets are provided by the module's own Makefile
# No default implementations needed - prevents override warnings

endif

# =============================================================================
# SHARED CLEAN TARGET (for all module types)
# =============================================================================

ifneq ($(IS_GO_MODULE),1)
# Basic clean target for non-Go modules
clean:
	@echo "Cleaning $(MODULE_NAME)..."
	rm -f coverage.txt coverage.xml report.xml code-quality-report.json $(CLEAN_EXTRA_FILES)
endif

# =============================================================================
# STANDARDIZED DOCKER TARGETS (conditional based on HAS_DOCKER)
# =============================================================================

ifeq ($(HAS_DOCKER),1)

# Setup buildx builder (standardized across all modules)
setup-buildx:
	@echo "Setting up Docker buildx builder for $(MODULE_NAME)..."
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		(docker context create $(BUILDX_BUILDER)-context || true && \
		 docker buildx create --name $(BUILDX_BUILDER) $(BUILDX_BUILDER)-context --driver docker-container --driver-opt network=host --buildkitd-flags '--allow-insecure-entitlement network.host' && \
		 docker buildx use $(BUILDX_BUILDER) && \
		 docker buildx inspect --bootstrap)

# Standardized Docker build (always from repo root for consistency)
docker-build: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME) (local development)..."
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

# Local-only Docker build (no remote cache, faster for local development)
docker-build-local: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME) (local, no remote cache)..."
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(DOCKER_EXTRA_ARGS) \
		$(DOCKER_LOAD_ARG) \
		-t $(MODULE_NAME):local \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

# Standardized Docker publish
docker-publish: setup-buildx
	@echo "Building and publishing Docker image for $(MODULE_NAME) (production)..."
	$(if $(filter true,$(DISABLE_REGISTRY_CACHE)),@echo "Registry cache disabled for this build")
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		$(CACHE_FROM_ARG) \
		$(CACHE_TO_ARG) \
		$(DOCKER_EXTRA_ARGS) \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-$(MODULE_NAME):$(SAFE_REF_NAME) \
		-f $(DOCKER_MODULE_PATH)/Dockerfile \
		.

# Legacy targets for backwards compatibility
image: docker-build
	@echo "Legacy 'image' target - use 'docker-build' for local development"

publish: docker-publish
	@echo "Legacy 'publish' target - use 'docker-publish' for CI/production"

else

# Placeholder targets for modules without Docker
docker-build docker-build-local docker-publish image publish setup-buildx:
	@echo "$(MODULE_NAME) does not support Docker builds"
	@exit 1

endif

# =============================================================================
# STANDARDIZED UTILITY TARGETS
# =============================================================================

# Standardized help target
help:
	@echo "$(MODULE_NAME) Makefile - Using nvsentinel common.mk standards"
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
	@echo "  lint-test  - Run full lint and test suite (matches CI)"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet        - Run go vet"
	@echo "  lint       - Run golangci-lint"
	@echo "  test       - Run tests with coverage"
	@echo "  coverage   - Generate coverage reports"
	@echo "  build      - Build the module"
	@echo "  binary     - Build the main binary"
	@echo ""
ifeq ($(HAS_DOCKER),1)
	@echo "Docker targets:"
	@echo "  docker-build       - Build Docker image (local development, with remote cache)"
	@echo "  docker-build-local - Build Docker image (local only, no remote cache)"
	@echo "  docker-publish     - Build and publish Docker image (CI/production)"
	@echo "  setup-buildx       - Setup Docker buildx builder"
	@echo "  image              - Legacy target (calls docker-build)"
	@echo "  publish            - Legacy target (calls docker-publish)"
	@echo ""
endif
	@echo "Utility targets:"
	@echo "  clean      - Clean build artifacts and reports"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Notes:"
	@echo "  - All Docker builds use repo-root context for consistency"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Images tagged with dynamic ref name: $(SAFE_REF_NAME)"
	@echo "  - Build cache is used for faster builds"
	@echo "  - Standardized across all nvsentinel modules"

# =============================================================================
# MODULE-SPECIFIC OVERRIDES
# =============================================================================
# Individual module Makefiles can override any target after including this file
# Example:
#   include ../common.mk
#
#   # Override test target for special requirements
#   test:
#   	@echo "Custom test implementation"
#   	$(SPECIAL_TEST_COMMAND)
