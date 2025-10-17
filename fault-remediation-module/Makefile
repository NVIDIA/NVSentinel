# Fault Remediation Module Makefile
# Individual module build and test targets (requires private repo access and kubebuilder)

# Copyright (c) 2024, NVIDIA CORPORATION. All rights reserved.
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

# Fault-remediation-module specific settings
CLEAN_EXTRA_FILES := fault-remediation-module

# Special kubebuilder setup for tests
TEST_SETUP_COMMANDS := export PATH="$$GOPATH/bin:$$PATH" && \
                      go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest && \
                      export KUBEBUILDER_ASSETS=$$(setup-envtest use -p path 1.30.0) &&

# Docker configuration
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../common.mk

# =============================================================================
# LEGACY COMPATIBILITY
# =============================================================================

.PHONY: image publish
image: docker-build
publish: docker-publish

# =============================================================================
# MODULE HELP
# =============================================================================

help:
	@echo "Fault Remediation Module Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This module handles fault remediation and recovery operations."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo "  GOPATH=$(GOPATH)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (standardized default)"
	@echo "  lint-test  - Run full lint and test suite"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet, lint, test, coverage, build, binary"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build, docker-publish, image, publish"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Requires kubebuilder setup-envtest for testing"
	@echo "  - Uses standardized repo-root Docker build context"
