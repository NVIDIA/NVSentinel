# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

IMAGE_NAME ?= slinky-drainer
IMAGE_TAG ?= latest
IMG ?= $(IMAGE_NAME):$(IMAGE_TAG)

CONTROLLER_GEN ?= $(shell go env GOPATH)/bin/controller-gen

.PHONY: help
help:
	@echo "Available targets:"
	@echo "  build        - Build the binary"
	@echo "  generate     - Generate DeepCopy code and CRDs"
	@echo "  ko-build     - Build the image with ko"
	@echo "  ko-push      - Push the image with ko"
	@echo "  deploy       - Deploy to Kubernetes using kustomize"
	@echo "  undeploy     - Remove deployment from Kubernetes"
	@echo "  manifests    - Generate all manifests"
	@echo "  clean        - Clean build artifacts"
	@echo "  run          - Run the controller locally"
	@echo "  test         - Run tests"
	@echo "  lint         - Run golangci-lint"
	@echo "  lint-test    - Lint and test with coverage (CI)"

.PHONY: build
build:
	go build -o bin/slinky-drainer main.go

.PHONY: run
run:
	go run main.go --slinky-namespace=slinky --pod-check-interval=5s --drain-timeout=30m

.PHONY: generate
generate: controller-gen hack/boilerplate.go.txt
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=config/crd

hack/boilerplate.go.txt: ../../.github/headers/LICENSE
	@mkdir -p hack
	@echo "/*" > $@
	@cat $< >> $@
	@echo "*/" >> $@

.PHONY: controller-gen
controller-gen:
	@test -s $(CONTROLLER_GEN) || { echo "Installing controller-gen..."; go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest; }

.PHONY: ko-build
ko-build:
	KO_DOCKER_REPO=ko.local ko build --bare --local . --tags latest
	docker tag ko.local:latest $(IMG)

.PHONY: ko-push
ko-push:
	KO_DOCKER_REPO=$(shell echo $(IMG) | cut -d: -f1) ko build --bare . --tags $(shell echo $(IMG) | cut -d: -f2)

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
	rm -rf bin/ dist/ slinky-drainer

.PHONY: test
test:
	go test ./... -v

.PHONY: lint
lint:
	golangci-lint run --config ../../.golangci.yml

.PHONY: lint-test
lint-test: lint
	go vet ./... && \
	gotestsum --junitfile report.xml -- -race ./... -coverprofile=coverage.txt -covermode atomic -coverpkg=github.com/nvidia/nvsentinel/plugins/slinky-drainer/... && \
	go tool cover -func coverage.txt && \
	gocover-cobertura < coverage.txt > coverage.xml

