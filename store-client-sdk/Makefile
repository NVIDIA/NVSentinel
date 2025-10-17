# Store Client SDK Makefile
# Individual module build and test targets

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

# Store-client-sdk specific settings
HAS_DOCKER := 0

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../common.mk

# =============================================================================
# MODULE HELP
# =============================================================================

help:
	@echo "Store Client SDK Makefile - Using nvsentinel common.mk standards"
	@echo ""
	@echo "This is a library module providing datastore abstraction."
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  MODULE_NAME=$(MODULE_NAME)"
	@echo "  REPO_ROOT=$(REPO_ROOT)"
	@echo "  HAS_DOCKER=0 (library module - no Docker builds)"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint-test (standardized default)"
	@echo "  lint-test  - Run full lint and test suite"
	@echo ""
	@echo "Individual targets:"
	@echo "  vet, lint, test, coverage, build"
	@echo ""
	@echo "Note: Docker targets are disabled for this library module"
	@echo ""
	@echo "Module-specific notes:"
	@echo "  - Library module with pluggable datastore providers"
	@echo "  - No binary output or Docker builds"
	@echo "  - Uses only public Go dependencies"
