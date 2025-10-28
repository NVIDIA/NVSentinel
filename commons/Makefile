# Logger SDK Makefile
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

# commons specific settings (library module - no Docker or binary)
IS_GO_MODULE := 1
HAS_DOCKER := 0

# =============================================================================
# INCLUDE SHARED DEFINITIONS
# =============================================================================

include ../make/common.mk
include ../make/go.mk

# =============================================================================
# DEFAULT TARGET
# =============================================================================

.PHONY: all
all: lint-test ## Run lint-test (default target)

# =============================================================================
# MODULE NOTES
# =============================================================================
# This is a library module providing centralized logging initialization.
# - Library module providing structured logging utilities
# - No binary output or Docker builds
# - Used by all nvsentinel modules for consistent logging setup
# Run 'make help' to see available targets
