# labeler-module Makefile

# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION
# =============================================================================

IS_GO_MODULE := 1

# Test setup commands for kubebuilder envtest
TEST_SETUP_COMMANDS := \
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest && \
	eval $$(setup-envtest use --use-env -p env) &&

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../make/common.mk
include ../make/go.mk

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint-test

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "labeler-module Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo ""
	@echo "Build notes:"
	@echo "  - Container images are built using ko (see Tiltfile)"

# =============================================================================
# KO BUILD TARGETS
# =============================================================================

.PHONY: ko-build
ko-build: ## Build container image using ko
	@echo "Building container image with ko..."
	@cd $(REPO_ROOT) && KO_DOCKER_REPO=$${KO_DOCKER_REPO:-ko.local} ./scripts/buildko.sh

.PHONY: ko-publish
ko-publish: ## Build and publish container image using ko
	@echo "Building and publishing container image with ko..."
	@cd $(REPO_ROOT) && ./scripts/buildko.sh
