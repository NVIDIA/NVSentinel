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

PYTHON_PACKAGE_NAME := nccl_allreduce
MODULE_NAME := $(PYTHON_PACKAGE_NAME)

include ../../make/common.mk
include ../../make/python.mk
include ../../make/docker.mk

PYTORCH_VERSION ?= 25.01-py3
IMAGE_NAME := preflight-nccl-allreduce

.PHONY: all
all: lint-test

.PHONY: protos-clean
protos-clean:
	@echo "Cleaning generated protobuf files in $(MODULE_NAME)..."
	find $(PYTHON_PACKAGE_NAME)/protos/ \( -name "*_pb2.py" -o -name "*_pb2_grpc.py" -o -name "*_pb2.pyi" \) -type f -delete 2>/dev/null || true

.PHONY: protos-generate
protos-generate: protos-clean
	@echo "Generating protobuf files for $(MODULE_NAME)..."
	@mkdir -p $(PYTHON_PACKAGE_NAME)/protos
	python3 -m grpc_tools.protoc \
		-I $(REPO_ROOT)/data-models/protobufs/ \
		--python_out=$(PYTHON_PACKAGE_NAME)/protos \
		--pyi_out=$(PYTHON_PACKAGE_NAME)/protos \
		--grpc_python_out=$(PYTHON_PACKAGE_NAME)/protos \
		$(REPO_ROOT)/data-models/protobufs/health_event.proto
	@# Fix relative imports in generated gRPC files
	@if command -v gsed >/dev/null 2>&1; then \
		gsed -i 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2_grpc.py; \
	elif [ "$$(uname)" = "Darwin" ]; then \
		sed -i '' 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2_grpc.py; \
	else \
		sed -i 's/^import health_event_pb2 as health__event__pb2$$/from . import health_event_pb2 as health__event__pb2/' $(PYTHON_PACKAGE_NAME)/protos/health_event_pb2_grpc.py; \
	fi
	@touch $(PYTHON_PACKAGE_NAME)/protos/__init__.py

.PHONY: setup
setup:
	@echo "Setting up Poetry environment for $(MODULE_NAME)..."
	poetry config virtualenvs.in-project true
	poetry install

docker-build: setup-buildx
	@echo "Building Docker image for $(MODULE_NAME)..."
	cd $(REPO_ROOT) && docker buildx build \
		-f $(MODULE_PATH)/Dockerfile \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/$(IMAGE_NAME):$(SAFE_REF_NAME) \
		--build-arg PYTORCH_VERSION=$(PYTORCH_VERSION) \
		--network=host \
		$(DOCKER_LOAD_ARG) \
		.

docker-publish: setup-buildx
	@echo "Publishing Docker image for $(MODULE_NAME)..."
	cd $(REPO_ROOT) && docker buildx build \
		-f $(MODULE_PATH)/Dockerfile \
		-t $(CONTAINER_REGISTRY)/$(CONTAINER_ORG)/nvsentinel/$(IMAGE_NAME):$(SAFE_REF_NAME) \
		--build-arg PYTORCH_VERSION=$(PYTORCH_VERSION) \
		--network=host \
		--push \
		.
