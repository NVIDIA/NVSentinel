# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

IS_GO_MODULE := 0
HAS_DOCKER := 1

MODULE_NAME := cuda-validation

include ../../make/common.mk
include ../../make/docker.mk

PYTORCH_VERSION ?= 26.04-py3
IMAGE_NAME := preflight-cuda-validation

.PHONY: all
all: lint-test

# Lint + test using the system Python (no Poetry: this module is a
# single-file probe, no package or test suite yet). Performs a syntax
# check via ast.parse so CI flags any obvious breakage.
.PHONY: lint-test
lint-test:
	@echo "Syntax-checking cuda_validation.py..."
	python3 -c "import ast; ast.parse(open('cuda_validation.py').read())"
	@echo "OK"

.PHONY: ci-test
ci-test: lint-test

# Local Docker build (override default docker.mk target so we can pass
# PYTORCH_VERSION matching the sibling preflight-checks idiom).
docker-build: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME)..."
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		-f $(MODULE_PATH)/Dockerfile \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/$(IMAGE_NAME):$(SAFE_REF_NAME) \
		--build-arg PYTORCH_VERSION=$(PYTORCH_VERSION) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--network=host \
		$(DOCKER_LOAD_ARG) \
		.

docker-publish: setup-buildx
	@echo "Publishing Docker image for $(MODULE_NAME)..."
	cd $(REPO_ROOT) && docker buildx build \
		--platform $(PLATFORMS) \
		-f $(MODULE_PATH)/Dockerfile \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/$(IMAGE_NAME):$(SAFE_REF_NAME) \
		--build-arg PYTORCH_VERSION=$(PYTORCH_VERSION) \
		--build-arg VERSION=$(VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		--network=host \
		--push \
		.
