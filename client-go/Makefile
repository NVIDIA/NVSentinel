#  Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
# 
#  Licensed under the Apache License, Version 2.0 (the "License");
#  you may not use this file except in compliance with the License.
#  You may obtain a copy of the License at
# 
#       http://www.apache.org/licenses/LICENSE-2.0
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

# Versioning
GIT_VERSION     ?= $(shell git describe --tags --always --dirty)
VERSION_PACKAGE := github.com/nvidia/nvsentinel/client-go/version
VERSION_FLAGS   := -X '$(VERSION_PACKAGE).GitVersion=$(GIT_VERSION)'

# Linker Flags
EXTRA_LDFLAGS ?=
LDFLAGS       := -ldflags "$(VERSION_FLAGS) $(EXTRA_LDFLAGS)"

# Shell setup
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# ==============================================================================
# Targets
# ==============================================================================

.PHONY: all
all: code-gen test build ## Run code generation, compile all code, and execute tests.

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: code-gen
code-gen: ## Run code generation (Client, Listers, Informers).
	@echo "Generating code ($(MODULE_NAME))..."
	./hack/update-codegen.sh
	@echo "Synchronizing module dependencies..."
	go mod tidy

.PHONY: build
build: ## Compile all Go code after generation to verify type safety.
	@echo "Compiling ($(MODULE_NAME))..."
	go build -v $(LDFLAGS) $$(go list ./... | grep -vE '/hack/overlays/|/examples/|/tests/')

.PHONY: test
test: ## Run unit tests and generate coverage.
	@echo "Testing ($(MODULE_NAME))..."
	go test -v $$(go list ./... | grep -vE '/hack/overlays/|/client/|/listers/|informers/|/fake|/scheme|/typed/|/examples/') -coverprofile cover.out

.PHONY: lint
lint: ## Run golangci-lint.
	@echo "Linting ($(MODULE_NAME))..."
	golangci-lint run ./...

.PHONY: clean
clean: ## Remove all generated code.
	@echo "Cleaning generated code ($(MODULE_NAME))..."
	rm -rf client listers informers
