# pod-device-mapper Makefile

# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.

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

# The mapper tests start a Kubernetes API server with envtest.
TEST_SETUP_COMMANDS := \
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) && \
	eval $$(setup-envtest use --use-env -p env) &&

.PHONY: all
all: lint-test

.PHONY: help
help:
	@echo "pod-device-mapper Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo "Ko targets: ko-build, ko-publish"
