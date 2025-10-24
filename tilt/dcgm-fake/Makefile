# DCGM Fake Makefile
# Simple Docker container for DCGM testing

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

# DCGM fake specific settings (no binary to clean)
CLEAN_EXTRA_FILES :=

# Docker configuration
HAS_DOCKER := 1
DOCKER_EXTRA_ARGS :=

# =============================================================================
# INCLUDE COMMON DEFINITIONS
# =============================================================================

include ../../make/common.mk

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
	@echo "DCGM Fake Makefile - Using nvsentinel common.mk standards"
	@echo ""