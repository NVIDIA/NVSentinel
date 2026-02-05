#  Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
# 
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
# 
#      http://www.apache.org/licenses/LICENSE-2.0
# 
#  Unless required by applicable law or agreed to in writing, software
#  distributed under the License is distributed on an "AS IS" BASIS,
#  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#  See the License for the specific language governing permissions and
#  limitations under the License.

# ==============================================================================
# Configuration
# ==============================================================================

MODULE_NAME = $(shell basename $(shell pwd))

# Shell setup
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Tool versions
CONTROLLER_TOOLS_VERSION ?= v0.17.3

# Tool binaries
LOCALBIN ?= $(shell pwd)/bin
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen

# ==============================================================================
# Targets
# ==============================================================================

.PHONY: all
all: code-gen manifests test build ## Run code generation, compile all code, and execute tests.

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: code-gen
code-gen: ## Run all code generation targets (deepcopy, protobuf).
	@echo "Generating code ($(MODULE_NAME))..."
	./hack/update-codegen.sh
	@echo "Synchronizing module dependencies..."
	go mod tidy

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests using controller-gen.
	@echo "Generating CRD manifests..."
	$(CONTROLLER_GEN) crd paths="./nvsentinel/..." output:crd:artifacts:config=../deployments/helm/nvsentinel/crds

.PHONY: generate
generate: code-gen manifests ## Run all code generation (deepcopy + CRD manifests).

.PHONY: build
build: ## Compile all Go code after generation to verify type safety.
	@echo "Compiling ($(MODULE_NAME))..."
	go build -v ./...

.PHONY: test
test: ## Run unit tests and generate coverage after code generation.
	@echo "Testing ($(MODULE_NAME))..."
	go test -v $$(go list ./... | grep -v /gen/) -coverprofile cover.out

.PHONY: lint
lint: ## Run golangci-lint.
	@echo "Linting ($(MODULE_NAME))..."
	golangci-lint run ./...

.PHONY: clean
clean: ## Remove all generated code.
	@echo "Cleaning generated code ($(MODULE_NAME))..."
	rm -rf gen/go
	find . -name "zz_generated.*.go" -delete

##@ Tools

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Install controller-gen locally.

$(CONTROLLER_GEN): $(LOCALBIN)
	@echo "Installing controller-gen $(CONTROLLER_TOOLS_VERSION)..."
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)
