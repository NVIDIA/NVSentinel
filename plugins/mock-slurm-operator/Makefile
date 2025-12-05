# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

IMAGE_NAME ?= mock-slurm-operator
IMAGE_TAG ?= latest
IMAGE_REGISTRY ?= localhost:5000

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build        - Build the binary"
	@echo "  ko-build     - Build the image with ko"
	@echo "  ko-push      - Push the image with ko"
	@echo "  deploy       - Deploy to Kubernetes using kustomize"
	@echo "  undeploy     - Remove deployment from Kubernetes"
	@echo "  manifests    - Generate all manifests"
	@echo "  clean        - Clean build artifacts"

.PHONY: build
build:
	mkdir -p bin
	go build -o bin/mock-slurm-operator main.go

.PHONY: ko-build
ko-build:
	KO_DOCKER_REPO=ko.local ko build --bare --local . --tags $(IMAGE_TAG)
	docker tag ko.local:$(IMAGE_TAG) $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(IMAGE_TAG)

.PHONY: ko-push
ko-push:
	KO_DOCKER_REPO=$(IMAGE_REGISTRY)/$(IMAGE_NAME) ko build --bare . --tags $(IMAGE_TAG)

.PHONY: manifests
manifests:
	mkdir -p dist
	kubectl kustomize config/default > dist/install.yaml

.PHONY: deploy
deploy: manifests
	kubectl apply -f dist/install.yaml

.PHONY: undeploy
undeploy:
	kubectl delete -f dist/install.yaml --ignore-not-found=true

.PHONY: clean
clean:
	rm -rf bin dist mock-slurm-operator
