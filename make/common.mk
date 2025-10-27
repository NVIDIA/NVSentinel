# make/common.mk - Shared variables for all nvsentinel modules
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
#
# This file provides standardized variables used across all modules.
# Include this file in individual module Makefiles to ensure consistency.

# =============================================================================
# REPOSITORY AND MODULE DETECTION
# =============================================================================

# Repository root path calculation (works from any subdirectory depth)
REPO_ROOT := $(shell git rev-parse --show-toplevel)

# Auto-detect current module name from directory
MODULE_NAME := $(shell basename $(CURDIR))

# Auto-detect module path relative to repo root
MODULE_PATH := $(subst $(REPO_ROOT)/,,$(CURDIR))

# =============================================================================
# DOCKER CONFIGURATION
# =============================================================================

# Docker registry and organization (set from environment in CI)
CONTAINER_REGISTRY ?= ghcr.io
CONTAINER_ORG      ?= nvidia

# Git branch handling for image tags
CI_COMMIT_REF_NAME ?= $(shell git rev-parse --abbrev-ref HEAD)
SAFE_REF_NAME ?= $(shell echo $(CI_COMMIT_REF_NAME) | sed 's/\//-/g')
SAFE_REF_NAME := $(if $(SAFE_REF_NAME),$(SAFE_REF_NAME),local)

# Docker buildx configuration
BUILDX_BUILDER ?= nvsentinel-builder
PLATFORMS ?= linux/arm64,linux/amd64

# Cache configuration (can be disabled via environment variables)
DISABLE_REGISTRY_CACHE ?= false
ifeq ($(DISABLE_REGISTRY_CACHE),true)
CACHE_FROM_ARG :=
CACHE_TO_ARG :=
else
CACHE_FROM_ARG := --cache-from=type=registry,ref=$(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel-buildcache:$(MODULE_NAME)
CACHE_TO_ARG := --cache-to=type=registry,ref=$(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel-buildcache:$(MODULE_NAME),mode=max
endif

# Docker build arguments
DOCKER_EXTRA_ARGS ?=
DOCKER_LOAD_ARG ?= --load

# =============================================================================
# TOOL CONFIGURATION
# =============================================================================

# Helper to read versions from .versions.yaml
VERSIONS_FILE := $(REPO_ROOT)/.versions.yaml
get-version = $(shell yq '.$(1)' $(VERSIONS_FILE) 2>/dev/null || echo "")

# Setup-envtest version (used in modules with kubebuilder tests)
SETUP_ENVTEST_VERSION := $(call get-version,go_tools.setup_envtest)

# Go binary and tools (standardized versions)
GO := go
GOLANGCI_LINT := golangci-lint
GOTESTSUM := gotestsum
GOCOVER_COBERTURA := gocover-cobertura

# Auto-detect golangci-lint config path (handles nested modules)
GOLANGCI_CONFIG_PATH := $(REPO_ROOT)/.golangci.yml

# =============================================================================
# MODULE TYPE CONFIGURATION (set by individual Makefiles)
# =============================================================================

# Module type configuration (Go=1, Python=0 - Python modules override targets)
IS_GO_MODULE ?= 1

# Docker configuration (can be overridden per module)
HAS_DOCKER ?= 0

# =============================================================================
# CONFIGURABLE FLAGS (defaults, can be overridden by modules)
# =============================================================================

# Configurable lint flags (defaults to code-climate output)
LINT_EXTRA_FLAGS ?= --out-format code-climate:code-quality-report.json,colored-line-number

# Configurable test flags (e.g., TEST_EXTRA_FLAGS := -short)
TEST_EXTRA_FLAGS ?=

# Binary configuration (defaults to module name and current directory)
BINARY_TARGET ?= $(MODULE_NAME)
BINARY_SOURCE ?= .

# Additional clean files (module-specific artifacts)
CLEAN_EXTRA_FILES ?=

# Test setup commands (e.g., for kubebuilder)
TEST_SETUP_COMMANDS ?=
