# csp-health-monitor Makefile

# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION
# =============================================================================

IS_GO_MODULE := 1
IS_KO_MODULE := 1

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../../make/common.mk
include ../../make/go.mk

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
	@echo "csp-health-monitor Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo "Ko targets: ko-build, ko-publish"
	@echo ""
	@echo "Build notes:"
	@echo "  - Container images are built using ko"
	@echo "  - Use 'make ko-build' for local builds (KO_DOCKER_REPO=ko.local by default)"
	@echo "  - Use 'make ko-publish' to build and push (set KO_DOCKER_REPO and VERSION)"
	@echo "  - For Tilt development, see Tiltfile"

