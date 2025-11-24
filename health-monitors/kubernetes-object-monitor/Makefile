# kubernetes-object-monitor Makefile

# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.

IS_GO_MODULE := 1
HAS_DOCKER := 1

include ../../make/common.mk
include ../../make/go.mk
include ../../make/docker.mk

# Test setup commands for kubebuilder envtest
# Version is centrally managed in .versions.yaml
TEST_SETUP_COMMANDS := \
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION) && \
	eval $$(setup-envtest use --use-env -p env) &&

.PHONY: all
all: lint-test

.PHONY: help
help:
	@echo "kubernetes-object-monitor Makefile - Using nvsentinel make/*.mk standards"
	@echo ""
	@echo "Main targets: all, lint-test, ci-test, build, test, lint, clean"
	@echo "Docker targets: docker, docker-build, docker-publish"

