# Main Makefile for nvsentinel project
# Coordinates between multiple sub-Makefiles organized by functionality

# Go binary and tools
GO := go
GOLANGCI_LINT := golangci-lint
GOTESTSUM := gotestsum
GOCOVER_COBERTURA := gocover-cobertura
ENVTEST := setup-envtest

# Variables
GOPATH ?= $(shell go env GOPATH)
GO_CACHE_DIR ?= $(shell go env GOCACHE)

# Go modules with specific patterns from CI
GO_MODULES := \
	health-monitors/nic-health-monitor \
	health-monitors/syslog-health-monitor \
	health-monitors/nvswitch-health-monitor \
	health-monitors/csp-health-monitor \
	health-event-client \
	platform-connectors \
	health-events-analyzer \
	fault-quarantine-module \
	labeler-module \
	node-drainer-module \
	fault-remediation-module \
	store-client-sdk

# Python modules
PYTHON_MODULES := \
	health-monitors/gpu-health-monitor

# Container-only modules
CONTAINER_MODULES := \
	nvsentinel-log-collector

# Special modules requiring private repo access
PRIVATE_MODULES := \
	health-monitors/csp-health-monitor \
	health-events-analyzer \
	fault-quarantine-module \
	labeler-module \
	node-drainer-module \
	fault-remediation-module

# Modules requiring kubebuilder for tests
KUBEBUILDER_MODULES := \
	node-drainer-module \
	fault-remediation-module

# Default target
.PHONY: all
all: lint-test-all

# Lint and test all modules (delegates to sub-Makefiles)
.PHONY: lint-test-all
lint-test-all: protos-lint license-headers-lint health-monitors-lint-test-all go-lint-test-all python-lint-test-all kubernetes-distro-lint log-collector-lint

# Health monitors lint-test (delegate to health-monitors/Makefile)
.PHONY: health-monitors-lint-test-all
health-monitors-lint-test-all:
	@echo "Running lint and tests for all health monitors..."
	$(MAKE) -C health-monitors lint-test-all

# Generate protobuf files
.PHONY: protos-generate
protos-generate:
	@echo "Generating protobuf files..."
	protoc -I protobufs/ --go_out=platform-connectors/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=platform-connectors/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=health-monitors/nic-health-monitor/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-monitors/nic-health-monitor/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=health-monitors/nvswitch-health-monitor/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-monitors/nvswitch-health-monitor/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos/ protobufs/nodehealtheventsudsconnector.proto
	python3 -m grpc_tools.protoc -Iprotobufs/ --python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos --pyi_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos --grpc_python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos protobufs/platformconnector.proto
	python3 -m grpc_tools.protoc -Iprotobufs/ --python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos --pyi_out=health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos --grpc_python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos protobufs/platformconnector.proto
	sed -i 's/^import platformconnector_pb2 as platformconnector__pb2$$/from . import platformconnector_pb2 as platformconnector__pb2/' health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos/platformconnector_pb2_grpc.py
	sed -i 's/^import platformconnector_pb2 as platformconnector__pb2$$/from . import platformconnector_pb2 as platformconnector__pb2/' health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos/platformconnector_pb2_grpc.py
	git status --porcelain --untracked-files=no
	git --no-pager diff
	@echo "Checking if protobuf files are up to date..."
	test -z "$$(git status --porcelain --untracked-files=no)"

# Check protobuf files
.PHONY: protos-lint
protos-lint: protos-generate
	@echo "Checking protobuf files..."
	protoc -I protobufs/ --go_out=platform-connectors/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=platform-connectors/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=health-monitors/nic-health-monitor/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-monitors/nic-health-monitor/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=health-monitors/nvswitch-health-monitor/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-monitors/nvswitch-health-monitor/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=platform-connectors/pkg/connectors/nodehealtheventsudsconnector/protos/ protobufs/nodehealtheventsudsconnector.proto
	python3 -m grpc_tools.protoc -Iprotobufs/ --python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos --pyi_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos --grpc_python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos protobufs/platformconnector.proto
	python3 -m grpc_tools.protoc -Iprotobufs/ --python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos --pyi_out=health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos --grpc_python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos protobufs/platformconnector.proto
	sed -i 's/^import platformconnector_pb2 as platformconnector__pb2$$/from . import platformconnector_pb2 as platformconnector__pb2/' health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos/platformconnector_pb2_grpc.py
	sed -i 's/^import platformconnector_pb2 as platformconnector__pb2$$/from . import platformconnector_pb2 as platformconnector__pb2/' health-monitors/gpu-health-monitor/gpu_health_monitor/clear_xid_errors/protos/platformconnector_pb2_grpc.py
	git status --porcelain --untracked-files=no
	git --no-pager diff
	@echo "Checking if protobuf files are up to date..."
	test -z "$$(git status --porcelain --untracked-files=no)"

# Check license headers
.PHONY: license-headers-lint
license-headers-lint:
	@echo "Checking license headers..."
	addlicense -f license-header.txt -check -ignore **/*lock.hcl -ignore **/*pb2.py -ignore **/*pb2_grpc.py -ignore **/*.csv -ignore **/.venv/** -ignore distros/kubernetes/nvsentinel/charts/mongodb-store/charts/mongodb/Chart.yaml -ignore distros/kubernetes/nvsentinel/charts/mongodb-store/charts/mongodb/charts/common/Chart.yaml -ignore health-monitors/gpu-health-monitor/pyproject.toml -ignore nvsentinel-log-collector/pyproject.toml .

# Lint and test non-health-monitor Go modules
.PHONY: go-lint-test-all
go-lint-test-all:
	@echo "Running lint and tests for non-health-monitor Go modules..."
	@for module in $(shell echo "$(GO_MODULES)" | tr ' ' '\n' | grep -v health-monitors); do \
		echo "Processing $$module..."; \
		$(MAKE) lint-test-$$module || exit 1; \
	done

# Lint and test non-health-monitor Python modules
.PHONY: python-lint-test-all
python-lint-test-all:
	@echo "Running lint and tests for non-health-monitor Python modules..."
	@for module in $(shell echo "$(PYTHON_MODULES)" | tr ' ' '\n' | grep -v health-monitors); do \
		echo "Processing $$module..."; \
		$(MAKE) lint-test-$$module || exit 1; \
	done

# Individual non-health-monitor Go module lint-test targets

.PHONY: lint-test-health-event-client
lint-test-health-event-client:
	@echo "Linting and testing health-event-client..."
	cd health-event-client && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race -short $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-platform-connectors
lint-test-platform-connectors:
	@echo "Linting and testing platform-connectors..."
	cd platform-connectors && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race -short $$(go list ./... | grep -v -e pkg/protos -e pkg/connectors/nodehealtheventsudsconnector/protos) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-health-events-analyzer
lint-test-health-events-analyzer:
	@echo "Linting and testing health-events-analyzer (with private repos)..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	cd health-events-analyzer && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-fault-quarantine-module
lint-test-fault-quarantine-module:
	@echo "Linting and testing fault-quarantine-module (with private repos)..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	cd fault-quarantine-module && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-labeler-module
lint-test-labeler-module:
	@echo "Linting and testing labeler-module (with private repos)..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	cd labeler-module && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use -p path)" $(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-node-drainer-module
lint-test-node-drainer-module:
	@echo "Linting and testing node-drainer-module (with private repos and kubebuilder)..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	export PATH="$$GOPATH/bin:$$PATH" && \
	$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest && \
	export KUBEBUILDER_ASSETS=$$(setup-envtest use -p path 1.30.0) && \
	cd node-drainer-module && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-fault-remediation-module
lint-test-fault-remediation-module:
	@echo "Linting and testing fault-remediation-module (with private repos and kubebuilder)..."
	export GOPRIVATE=gitlab-master.nvidia.com/dgxcloud/mk8s/* && \
	export PATH="$$GOPATH/bin:$$PATH" && \
	$(GO) install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest && \
	export KUBEBUILDER_ASSETS=$$(setup-envtest use -p path 1.30.0) && \
	cd fault-remediation-module && \
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

.PHONY: lint-test-store-client-sdk
lint-test-store-client-sdk:
	@echo "Linting and testing store-client-sdk..."
	$(MAKE) -C store-client-sdk lint-test

# Python module lint-test targets (non-health-monitors)
# Currently no non-health-monitor Python modules

# Kubernetes distro lint (delegate to distros/kubernetes/Makefile)
.PHONY: kubernetes-distro-lint
kubernetes-distro-lint:
	@echo "Linting Kubernetes distribution..."
	$(MAKE) -C distros/kubernetes lint

# Log collector lint (shell script)
.PHONY: log-collector-lint
log-collector-lint:
	@echo "Linting log collector shell scripts..."
	$(MAKE) -C nvsentinel-log-collector lint

# Build targets (delegate to sub-Makefiles for better organization)
.PHONY: build-all
build-all: build-health-monitors build-main-modules

# Build health monitors (delegate to health-monitors/Makefile)
.PHONY: build-health-monitors
build-health-monitors:
	@echo "Building all health monitors..."
	$(MAKE) -C health-monitors build-all

# Build non-health-monitor Go modules
.PHONY: build-main-modules
build-main-modules:
	@echo "Building non-health-monitor Go modules..."
	@for module in $(shell echo "$(GO_MODULES)" | tr ' ' '\n' | grep -v health-monitors); do \
		echo "Building $$module..."; \
		cd $$module && $(GO) build ./... && cd ..; \
	done

# Individual build targets for non-health-monitor modules
define make-build-target
.PHONY: build-$(1)
build-$(1):
	@echo "Building $(1)..."
	cd $(1) && $(GO) build ./...
endef

$(foreach module,$(shell echo "$(GO_MODULES)" | tr ' ' '\n' | grep -v health-monitors),$(eval $(call make-build-target,$(module))))

# Health monitor build targets (delegate to health-monitors/Makefile)
.PHONY: build-nic-health-monitor
build-nic-health-monitor:
	$(MAKE) -C health-monitors build-nic-health-monitor

.PHONY: build-syslog-health-monitor
build-syslog-health-monitor:
	$(MAKE) -C health-monitors build-syslog-health-monitor

.PHONY: build-nvswitch-health-monitor
build-nvswitch-health-monitor:
	$(MAKE) -C health-monitors build-nvswitch-health-monitor

.PHONY: build-csp-health-monitor
build-csp-health-monitor:
	$(MAKE) -C health-monitors build-csp-health-monitor

.PHONY: build-gpu-health-monitor
build-gpu-health-monitor:
	$(MAKE) -C health-monitors build-gpu-health-monitor

# Clean targets (delegate to sub-Makefiles for better organization)
.PHONY: clean-all
clean-all: clean-health-monitors clean-main-modules

# Clean health monitors (delegate to health-monitors/Makefile)
.PHONY: clean-health-monitors
clean-health-monitors:
	@echo "Cleaning all health monitors..."
	$(MAKE) -C health-monitors clean-all

# Clean non-health-monitor Go modules
.PHONY: clean-main-modules
clean-main-modules:
	@echo "Cleaning non-health-monitor Go modules..."
	@for module in $(shell echo "$(GO_MODULES)" | tr ' ' '\n' | grep -v health-monitors); do \
		echo "Cleaning $$module..."; \
		cd $$module && $(GO) clean ./... && cd ..; \
	done

# Docker targets (delegate to docker/Makefile) - matching GitLab CI exactly
.PHONY: docker-all
docker-all:
	@echo "Building all Docker images..."
	$(MAKE) -C docker build-all

.PHONY: docker-publish-all
docker-publish-all:
	@echo "Building and publishing all Docker images..."
	$(MAKE) -C docker publish-all

.PHONY: docker-setup-buildx
docker-setup-buildx:
	$(MAKE) -C docker setup-buildx

# GPU health monitor Docker targets (special cases with DCGM versions)
.PHONY: docker-gpu-health-monitor-dcgm3
docker-gpu-health-monitor-dcgm3:
	$(MAKE) -C docker build-gpu-health-monitor-dcgm3

.PHONY: docker-gpu-health-monitor-dcgm4
docker-gpu-health-monitor-dcgm4:
	$(MAKE) -C docker build-gpu-health-monitor-dcgm4

.PHONY: docker-gpu-health-monitor
docker-gpu-health-monitor:
	$(MAKE) -C docker build-gpu-health-monitor

# Individual module Docker targets
.PHONY: docker-nic-health-monitor
docker-nic-health-monitor:
	$(MAKE) -C docker build-nic-health-monitor

.PHONY: docker-syslog-health-monitor
docker-syslog-health-monitor:
	$(MAKE) -C docker build-syslog-health-monitor

.PHONY: docker-nvswitch-health-monitor
docker-nvswitch-health-monitor:
	$(MAKE) -C docker build-nvswitch-health-monitor

.PHONY: docker-csp-health-monitor
docker-csp-health-monitor:
	$(MAKE) -C docker build-csp-health-monitor

.PHONY: docker-health-event-client
docker-health-event-client:
	$(MAKE) -C docker build-health-event-client

.PHONY: docker-platform-connectors
docker-platform-connectors:
	$(MAKE) -C docker build-platform-connectors

.PHONY: docker-health-events-analyzer
docker-health-events-analyzer:
	$(MAKE) -C docker build-health-events-analyzer

.PHONY: docker-fault-quarantine-module
docker-fault-quarantine-module:
	$(MAKE) -C docker build-fault-quarantine-module

.PHONY: docker-labeler-module
docker-labeler-module:
	$(MAKE) -C docker build-labeler-module

.PHONY: docker-node-drainer-module
docker-node-drainer-module:
	$(MAKE) -C docker build-node-drainer-module

.PHONY: docker-fault-remediation-module
docker-fault-remediation-module:
	$(MAKE) -C docker build-fault-remediation-module

.PHONY: docker-log-collector
docker-log-collector:
	$(MAKE) -C docker build-log-collector

# Health monitors group
.PHONY: docker-health-monitors
docker-health-monitors:
	$(MAKE) -C docker build-health-monitors

# Main modules group (non-health-monitors)
.PHONY: docker-main-modules
docker-main-modules:
	$(MAKE) -C docker build-main-modules

# Development environment targets (delegate to dev/Makefile)
.PHONY: tilt-up
tilt-up:
	$(MAKE) -C dev tilt-up

.PHONY: tilt-down
tilt-down:
	$(MAKE) -C dev tilt-down

.PHONY: tilt-ci
tilt-ci:
	$(MAKE) -C dev tilt-ci

.PHONY: cluster-create
cluster-create:
	$(MAKE) -C dev cluster-create

.PHONY: cluster-delete
cluster-delete:
	$(MAKE) -C dev cluster-delete

.PHONY: cluster-status
cluster-status:
	$(MAKE) -C dev cluster-status

.PHONY: dev-env
dev-env:
	$(MAKE) -C dev env-up

.PHONY: dev-env-clean
dev-env-clean:
	$(MAKE) -C dev env-down

# Tilt end-to-end test target for CI
.PHONY: e2e-test
e2e-test-ci:
	$(MAKE) -C dev tilt-ci
	$(MAKE) -C tests test-ci

# Tilt end-to-end test target
.PHONY: e2e-test
e2e-test:
	$(MAKE) -C dev tilt-up
	$(MAKE) -C tests test

# Kubernetes Helm targets (delegate to distros/kubernetes/Makefile)
.PHONY: kubernetes-distro-helm-publish
kubernetes-distro-helm-publish:
	$(MAKE) -C distros/kubernetes helm-publish

# Individual Docker build targets (delegate to docker/Makefile)
# Use: make -C docker build-<module-name>

# Utility targets
.PHONY: list-modules
list-modules:
	@echo "Go modules:"
	@for module in $(GO_MODULES); do echo "  $$module"; done
	@echo "Python modules:"
	@for module in $(PYTHON_MODULES); do echo "  $$module"; done
	@echo "Container-only modules:"
	@for module in $(CONTAINER_MODULES); do echo "  $$module"; done

.PHONY: help
help:
	@echo "nvsentinel Main Makefile - coordinates between multiple specialized sub-Makefiles"
	@echo ""
	@echo "Main targets:"
	@echo "  all                    - Run lint-test-all (default)"
	@echo "  lint-test-all          - Lint and test all modules"
	@echo "  protos-lint            - Generate and check protobuf files"
	@echo "  license-headers-lint   - Check license headers"
	@echo "  log-collector-lint     - Lint shell scripts"
	@echo ""
	@echo "Module-specific targets (delegated to sub-Makefiles):"
	@echo "  health-monitors-lint-test-all - Lint and test all health monitors"
	@echo "  go-lint-test-all              - Lint and test non-health-monitor Go modules"
	@echo "  python-lint-test-all          - Lint and test non-health-monitor Python modules"
	@echo "  kubernetes-distro-lint        - Lint Kubernetes Helm charts"
	@echo ""
	@echo "Development environment targets (delegated to dev/Makefile):"
	@echo "  dev-env                - Create cluster and start Tilt (full setup)"
	@echo "  dev-env-clean          - Stop Tilt and delete cluster (full cleanup)"
	@echo "  tilt-up                - Start Tilt development environment"
	@echo "  tilt-down              - Stop Tilt development environment"
	@echo "  tilt-ci                - Run Tilt in CI mode (no UI)"
	@echo "  cluster-create         - Create local ctlptl-managed Kind cluster with registry"
	@echo "  cluster-delete         - Delete local ctlptl-managed cluster and registry"
	@echo "  cluster-status         - Show cluster and registry status"
	@echo ""
	@echo "Docker targets (delegated to docker/Makefile) - matching GitLab CI exactly:"
	@echo "  docker-all                      - Build all Docker images"
	@echo "  docker-publish-all              - Build and publish all Docker images"
	@echo "  docker-setup-buildx             - Setup Docker buildx builder"
	@echo "  docker-health-monitors          - Build all health monitor images"
	@echo "  docker-main-modules             - Build all main module images"
	@echo ""
	@echo "  Special GPU health monitor targets:"
	@echo "  docker-gpu-health-monitor       - Build both DCGM 3.x and 4.x GPU monitor images"
	@echo "  docker-gpu-health-monitor-dcgm3 - Build GPU monitor with DCGM 3.x"
	@echo "  docker-gpu-health-monitor-dcgm4 - Build GPU monitor with DCGM 4.x"
	@echo ""
	@echo "  Individual module Docker targets:"
	@echo "  docker-nic-health-monitor       - Build NIC health monitor"
	@echo "  docker-syslog-health-monitor    - Build syslog health monitor"
	@echo "  docker-nvswitch-health-monitor  - Build NVSwitch health monitor"
	@echo "  docker-csp-health-monitor       - Build CSP health monitor"
	@echo "  docker-health-event-client      - Build health event client"
	@echo "  docker-platform-connectors     - Build platform connectors"
	@echo "  docker-health-events-analyzer  - Build health events analyzer"
	@echo "  docker-fault-quarantine-module - Build fault quarantine module"
	@echo "  docker-labeler-module          - Build labeler module"
	@echo "  docker-node-drainer-module     - Build node drainer module"
	@echo "  docker-fault-remediation-module - Build fault remediation module"
	@echo "  docker-log-collector           - Build log collector"
	@echo ""
	@echo "Helm/Kubernetes targets (delegated to distros/kubernetes/Makefile):"
	@echo "  kubernetes-distro-helm-publish - Publish Helm chart (requires CI_COMMIT_TAG)"
	@echo ""
	@echo "Build targets (delegated to sub-Makefiles):"
	@echo "  build-all              - Build all modules (health monitors + main modules)"
	@echo "  build-health-monitors  - Build all health monitors"
	@echo "  build-main-modules     - Build non-health-monitor Go modules"
	@echo "  build-<module-name>    - Build specific module"
	@echo ""
	@echo "Test targets (delegated to sub-Makefiles):"
	@echo "  e2e-test-ci        - Run end-to-end test suite in CI mode"
	@echo "  e2e-test           - Run end-to-end test suite"
	@echo ""
	@echo "Clean targets (delegated to sub-Makefiles):"
	@echo "  clean-all              - Clean all modules"
	@echo "  clean-health-monitors  - Clean all health monitors"
	@echo "  clean-main-modules     - Clean non-health-monitor Go modules"
	@echo ""
	@echo "Utility targets:"
	@echo "  list-modules           - List all modules"
	@echo "  help                   - Show this help message"
	@echo ""
	@echo "Sub-Makefile locations:"
	@echo "  health-monitors/Makefile  - Health monitor specific targets"
	@echo "  distros/kubernetes/Makefile - Kubernetes/Helm specific targets"
	@echo "  docker/Makefile           - Docker build specific targets"
	@echo "  dev/Makefile              - Development environment targets"
	@echo "  tests/Makefile            - End-to-end and integration test targets"
	@echo ""
	@echo "Individual module targets:"
	@echo "  For health monitors: make -C health-monitors <target>"
	@echo "  For docker builds: make -C docker <target>"
	@echo "  For development: make -C dev <target>"
	@echo "  For kubernetes: make -C distros/kubernetes <target>"
	@echo "  For tests: make -C tests <target>"
	@echo ""
	@echo "Notes:"
	@echo "  - Each sub-Makefile has its own help target: make -C <dir> help"
	@echo "  - Docker builds use multi-platform (linux/arm64,linux/amd64) and build cache"
	@echo "  - Docker targets match .gitlab-ci.yml configuration exactly"
	@echo "  - Development clusters use ctlptl for declarative management"
	@echo "  - Environment variables: NVCR_CONTAINER_REPO, NGC_ORG, SAFE_REF_NAME, PLATFORMS"
