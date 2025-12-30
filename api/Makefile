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

# Directories
PROTO_DIR       := proto
GEN_DIR         := gen/go

# Shell setup
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# Tool Binaries
PROTOC_GEN_GO           ?= $(LOCALBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC      ?= $(LOCALBIN)/protoc-gen-go-grpc

# Tool Versions (load from ../.versions.yaml)
# Requires yq to be installed: brew install yq (macOS) or see https://github.com/mikefarah/yq
YQ := $(shell command -v yq 2> /dev/null)
ifndef YQ
$(error yq is required but not found. Install it with: brew install yq (macOS) or see https://github.com/mikefarah/yq)
endif

VERSIONS_FILE := ../.versions.yaml

PROTOC_GEN_GO_VERSION           := $(shell $(YQ) '.protobuf.protoc_gen_go' $(VERSIONS_FILE))
PROTOC_GEN_GO_GRPC_VERSION      := $(shell $(YQ) '.protobuf.protoc_gen_go_grpc' $(VERSIONS_FILE))

# ==============================================================================
# Targets
# ==============================================================================

.PHONY: all
all: protos-generate build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: build
build:
	go build -v ./...

.PHONY: protos-generate
protos-generate: $(PROTOC_GEN_GO) $(PROTOC_GEN_GO_GRPC) protos-clean ## Generate Go code from Proto definitions.
	@echo "Generating Proto code..."
	@mkdir -p $(GEN_DIR)
	cd proto && \
	protoc \
		-I . \
		-I ../$(THIRD_PARTY_DIR) \
		--plugin="protoc-gen-go=$(PROTOC_GEN_GO)" \
		--plugin="protoc-gen-go-grpc=$(PROTOC_GEN_GO_GRPC)" \
		--go_out=../$(GEN_DIR) \
		--go_opt=paths=source_relative \
		--go-grpc_out=../$(GEN_DIR) \
		--go-grpc_opt=paths=source_relative \
		device/v1alpha1/*.proto && \
	protoc \
		-I . \
		-I ../$(THIRD_PARTY_DIR) \
		--plugin="protoc-gen-go=$(PROTOC_GEN_GO)" \
		--plugin="protoc-gen-go-grpc=$(PROTOC_GEN_GO_GRPC)" \
		--go_out=../$(GEN_DIR) \
		--go_opt=paths=source_relative \
		--go-grpc_out=../$(GEN_DIR) \
		--go-grpc_opt=paths=source_relative \
		csp/v1alpha1/*.proto
	@echo "Cleaning up dependencies..."
	go mod tidy
	@echo "Done."

.PHONY: protos-clean
protos-clean: ## Remove generated code.
	@echo "Cleaning old generated Proto code..."
	rm -rf $(GEN_DIR)
	@echo "Done."

##@ Build Dependencies

$(PROTOC_GEN_GO):
	$(call go-install-tool,$(PROTOC_GEN_GO),google.golang.org/protobuf/cmd/protoc-gen-go,$(PROTOC_GEN_GO_VERSION))

$(PROTOC_GEN_GO_GRPC):
	$(call go-install-tool,$(PROTOC_GEN_GO_GRPC),google.golang.org/grpc/cmd/protoc-gen-go-grpc,$(PROTOC_GEN_GO_GRPC_VERSION))

# go-install-tool macro
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
mkdir -p $(LOCALBIN); \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
