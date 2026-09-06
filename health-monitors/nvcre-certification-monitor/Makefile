# nvcre-certification-monitor Makefile
# Individual module build and test targets

# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
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

# nvcre-certification-monitor specific settings
CLEAN_EXTRA_FILES := nvcre-certification-monitor

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../../make/common.mk
include ../../make/go.mk

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
	@echo "nvcre-certification-monitor Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo "Ko targets: ko-build, ko-publish"
	@echo ""
	@echo "Build notes:"
	@echo "  - Container images are built using ko"
	@echo "  - Use 'make ko-build' for local builds (KO_DOCKER_REPO=ko.local by default)"
	@echo "  - Use 'make ko-publish' to build and push (set KO_DOCKER_REPO and VERSION)"
	@echo "  - Platforms configured in .ko.yaml (linux/amd64, linux/arm64)"

# =============================================================================
# MODULE NOTES
# =============================================================================
# This module watches NVIDIA Cluster Readiness Engine Certification CRs and
# publishes per-(node, variant, reason) health events for failed nodes.
# Run 'make help' to see all available targets and build instructions.
