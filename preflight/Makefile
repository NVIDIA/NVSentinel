# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION
# =============================================================================

# Module type and capabilities
IS_GO_MODULE := 1
IS_KO_MODULE := 1

# preflight specific settings
CLEAN_EXTRA_FILES := preflight

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
all: lint-test ## Run lint-test (default target)

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "preflight Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo "Ko targets: ko-build, ko-publish"
	@echo ""
	@echo "Build notes:"
	@echo "  - Container images are built using ko"
	@echo "  - Use 'make ko-build' for local builds (KO_DOCKER_REPO=ko.local by default)"
	@echo "  - Use 'make ko-publish' to build and push (set KO_DOCKER_REPO and VERSION)"
	@echo "  - For Tilt development, see Tiltfile"
	@echo "  - Platforms configured in .ko.yaml (linux/amd64, linux/arm64)"

# =============================================================================
# MODULE NOTES
# =============================================================================
# This module provides a mutating webhook that injects preflight check
# init containers into GPU pods.
# Run 'make help' to see all available targets and build instructions.
