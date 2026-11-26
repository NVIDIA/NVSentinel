# nvhv-health-monitor Makefile

# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.

# =============================================================================
# MODULE-SPECIFIC CONFIGURATION
# =============================================================================

IS_GO_MODULE := 1
IS_KO_MODULE := 1

# Generated protobuf code is excluded from coverage
TEST_EXCLUDE_PACKAGES := pkg/verdictpb

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
# NVHV VERDICT API (vendored proto)
# =============================================================================

VERDICT_PB_PKG := github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor/pkg/verdictpb/health/v1

# Regenerate pkg/verdictpb from proto/health/v1/verdict.proto, which is a copy
# of NVHV's proto/health/v1/verdict.proto.
.PHONY: proto
proto:
	protoc -I proto \
		--go_out=pkg/verdictpb --go_opt=paths=source_relative --go_opt=Mhealth/v1/verdict.proto=$(VERDICT_PB_PKG) \
		--go-grpc_out=pkg/verdictpb --go-grpc_opt=paths=source_relative --go-grpc_opt=Mhealth/v1/verdict.proto=$(VERDICT_PB_PKG) \
		proto/health/v1/verdict.proto

# =============================================================================
# MODULE HELP
# =============================================================================

.PHONY: help
help:
	@echo "nvhv-health-monitor Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean, proto"
	@echo "Ko targets: ko-build, ko-publish"
