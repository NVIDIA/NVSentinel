# node-drainer-module Makefile

# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION
# =============================================================================

IS_GO_MODULE := 1
IS_KO_MODULE := 1

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../make/common.mk
include ../make/go.mk

# Test setup commands for kubebuilder envtest
# Version is centrally managed in .versions.yaml
TEST_SETUP_COMMANDS := \
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) && \
	eval $$(setup-envtest use --use-env -p env) &&

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
	@echo "node-drainer-module Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo "Ko targets: ko-build, ko-publish"
	@echo ""
	@echo "Build notes:"
	@echo "  - Container images are built using ko"
	@echo "  - Use 'make ko-build' for local builds (KO_DOCKER_REPO=ko.local by default)"
	@echo "  - Use 'make ko-publish' to build and push (set KO_DOCKER_REPO and VERSION)"
	@echo "  - For Tilt development, see Tiltfile"
