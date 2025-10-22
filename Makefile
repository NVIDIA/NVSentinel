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

# Tool versions
GOLANGCI_LINT_VERSION := v1.64.8
GOTESTSUM_VERSION := latest
GOCOVER_COBERTURA_VERSION := latest
GO_VERSION := 1.24.8
GRPCIO_TOOLS_VERSION := 1.75.1

# Go modules with specific patterns from CI
GO_MODULES := \
	health-monitors/syslog-health-monitor \
	health-monitors/csp-health-monitor \
	platform-connectors \
	health-events-analyzer \
	fault-quarantine-module \
	labeler-module \
	node-drainer-module \
	fault-remediation-module \
	store-client-sdk \
	statemanager

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

# Install lint tools
.PHONY: install-lint-tools
install-lint-tools: install-golangci-lint install-gotestsum install-gocover-cobertura
	@echo "All lint tools installed successfully"
	@echo ""
	@echo "=== Installed Tool Versions and Locations ==="
	@echo "Go: $$(go version)"
	@echo "    Location: $$(which go)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint: $$(golangci-lint version 2>/dev/null | head -1)"; \
		echo "    Location: $$(which golangci-lint)"; \
	else \
		echo "golangci-lint: not found"; \
	fi
	@if command -v gotestsum >/dev/null 2>&1; then \
		echo "gotestsum: $$(gotestsum --version 2>/dev/null || echo 'version command not available')"; \
		echo "    Location: $$(which gotestsum)"; \
	else \
		echo "gotestsum: not found"; \
	fi
	@if command -v gocover-cobertura >/dev/null 2>&1; then \
		echo "gocover-cobertura: installed (no version command available)"; \
		echo "    Location: $$(which gocover-cobertura)"; \
	else \
		echo "gocover-cobertura: not found"; \
	fi
	@echo "=============================================="

# Install golangci-lint
.PHONY: install-golangci-lint
install-golangci-lint:
	@echo "Installing golangci-lint $(GOLANGCI_LINT_VERSION)..."
	@if command -v golangci-lint >/dev/null 2>&1; then \
		current_version=$$(golangci-lint version 2>/dev/null | grep -o 'v[0-9]\+\.[0-9]\+\.[0-9]\+' || echo "unknown"); \
		if [ "$$current_version" = "$(GOLANGCI_LINT_VERSION)" ]; then \
			echo "golangci-lint $(GOLANGCI_LINT_VERSION) is already installed at $$(which golangci-lint)"; \
		else \
			existing_path=$$(which golangci-lint); \
			install_dir=$$(dirname "$$existing_path"); \
			echo "Current version: $$current_version at $$existing_path, installing $(GOLANGCI_LINT_VERSION) to $$install_dir..."; \
			curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b "$$install_dir" $(GOLANGCI_LINT_VERSION); \
		fi; \
	else \
		echo "golangci-lint not found, installing $(GOLANGCI_LINT_VERSION) to $(GOPATH)/bin..."; \
		curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(GOPATH)/bin $(GOLANGCI_LINT_VERSION); \
	fi
	@echo "golangci-lint installation complete"

# Install gotestsum
.PHONY: install-gotestsum
install-gotestsum:
	@echo "Installing gotestsum..."
	@if ! command -v gotestsum >/dev/null 2>&1; then \
		echo "gotestsum not found, installing..."; \
		$(GO) install gotest.tools/gotestsum@$(GOTESTSUM_VERSION); \
	else \
		echo "gotestsum is already installed"; \
	fi

# Install gocover-cobertura
.PHONY: install-gocover-cobertura
install-gocover-cobertura:
	@echo "Installing gocover-cobertura..."
	@if ! command -v gocover-cobertura >/dev/null 2>&1; then \
		echo "gocover-cobertura not found, installing..."; \
		$(GO) install github.com/boumenot/gocover-cobertura@$(GOCOVER_COBERTURA_VERSION); \
	else \
		echo "gocover-cobertura is already installed"; \
	fi

# Install Go $(GO_VERSION) for CI environments (Linux and macOS, amd64 and arm64)
.PHONY: install-go-ci
install-go-ci:
	@echo "Installing Go $(GO_VERSION) for CI..."
	@# Detect platform and architecture
	@OS=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	ARCH=$$(uname -m); \
	case "$$ARCH" in \
		x86_64) ARCH=amd64 ;; \
		aarch64|arm64) ARCH=arm64 ;; \
		*) echo "Unsupported architecture: $$ARCH" && exit 1 ;; \
	esac; \
	echo "Detected platform: $$OS-$$ARCH"; \
	\
	if command -v go >/dev/null 2>&1; then \
		current_version=$$(go version | grep -o 'go[0-9]\+\.[0-9]\+\.[0-9]\+' | sed 's/go//'); \
		if [ "$$current_version" = "$(GO_VERSION)" ]; then \
			echo "Go $(GO_VERSION) is already installed"; \
			echo "Location: $$(which go)"; \
			go version; \
			exit 0; \
		else \
			echo "Current Go version: $$current_version, installing $(GO_VERSION)..."; \
		fi; \
	else \
		echo "Go not found, installing $(GO_VERSION)..."; \
	fi; \
	\
	GO_TARBALL="go$(GO_VERSION).$$OS-$$ARCH.tar.gz"; \
	GO_URL="https://go.dev/dl/$$GO_TARBALL"; \
	echo "Downloading $$GO_URL..."; \
	\
	if command -v wget >/dev/null 2>&1; then \
		wget -q "$$GO_URL" || (echo "Failed to download Go tarball" && exit 1); \
	elif command -v curl >/dev/null 2>&1; then \
		curl -sSL "$$GO_URL" -o "$$GO_TARBALL" || (echo "Failed to download Go tarball" && exit 1); \
	else \
		echo "Neither wget nor curl found. Please install one of them." && exit 1; \
	fi; \
	\
	echo "Extracting Go $(GO_VERSION)..."; \
	if [ "$$OS" = "darwin" ]; then \
		rm -rf /usr/local/go 2>/dev/null || true; \
		tar -C /usr/local -xzf "$$GO_TARBALL"; \
		echo "Go $(GO_VERSION) installed to /usr/local/go"; \
		echo "Add /usr/local/go/bin to your PATH if not already present"; \
	else \
		rm -rf /usr/local/go 2>/dev/null || true; \
		tar -C /usr/local -xzf "$$GO_TARBALL"; \
		echo "Go $(GO_VERSION) installed to /usr/local/go"; \
		echo "Add /usr/local/go/bin to your PATH if not already present"; \
	fi; \
	\
	rm -f "$$GO_TARBALL"; \
	echo "Installation complete"; \
	\
	if [ -x /usr/local/go/bin/go ]; then \
		echo "Installed Go version: $$(/usr/local/go/bin/go version)"; \
		echo "Location: /usr/local/go/bin/go"; \
	else \
		echo "Warning: Go binary not found at expected location /usr/local/go/bin/go"; \
	fi

# Lint and test all modules (delegates to sub-Makefiles)
.PHONY: lint-test-all
lint-test-all: protos-lint license-headers-lint gomod-lint health-monitors-lint-test-all go-lint-test-all python-lint-test-all kubernetes-distro-lint log-collector-lint

# Health monitors lint-test (delegate to health-monitors/Makefile)
.PHONY: health-monitors-lint-test-all
health-monitors-lint-test-all:
	@echo "Running lint and tests for all health monitors..."
	$(MAKE) -C health-monitors lint-test-all

# Generate protobuf files
.PHONY: protos-generate
protos-generate: protos-clean
	@echo "Generating protobuf files..."
	@echo "=== Tool Versions ==="
	@echo "Go: $$(go version)"
	@echo "protoc: $$(protoc --version)"
	@echo "protoc-gen-go: $$(protoc-gen-go --version)"
	@echo "protoc-gen-go-grpc: $$(protoc-gen-go-grpc --version)"
	@echo "========================"
	protoc -I protobufs/ --go_out=platform-connectors/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=platform-connectors/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=health-monitors/syslog-health-monitor/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-monitors/syslog-health-monitor/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=health-events-analyzer/pkg/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=health-events-analyzer/pkg/protos/ protobufs/platformconnector.proto
	protoc -I protobufs/ --go_out=tilt/simple-health-client/protos/ --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative --go-grpc_out=tilt/simple-health-client/protos/ protobufs/platformconnector.proto
	python3 -m grpc_tools.protoc -Iprotobufs/ --python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos --pyi_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos --grpc_python_out=health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos protobufs/platformconnector.proto
	@SED_CMD=$$(command -v gsed 2>/dev/null || command -v sed); \
	$$SED_CMD -i 's/^import platformconnector_pb2 as platformconnector__pb2$$/from . import platformconnector_pb2 as platformconnector__pb2/' health-monitors/gpu-health-monitor/gpu_health_monitor/platform_connector/protos/platformconnector_pb2_grpc.py

# Check protobuf files
.PHONY: protos-lint
protos-lint: protos-generate
	@echo "Checking protobuf files..."
	git status --porcelain --untracked-files=no
	git --no-pager diff
	@echo "Checking if protobuf files are up to date..."
	test -z "$$(git status --porcelain --untracked-files=no)"

# Clean generated protobuf files
.PHONY: protos-clean
protos-clean:
	@echo "Cleaning generated protobuf files..."
	@echo "Removing Go protobuf files (.pb.go)..."
	find . -name "*.pb.go" -type f -delete
	@echo "Removing Python protobuf files (*_pb2.py, *_pb2_grpc.py, *_pb2.pyi)..."
	find . \( -name "*_pb2.py" -o -name "*_pb2_grpc.py" -o -name "*_pb2.pyi" \) -type f -delete
	@echo "All generated protobuf files have been removed."

# Check license headers
.PHONY: license-headers-lint
license-headers-lint:
	@echo "Checking license headers..."
	addlicense -f license-header.txt -check -ignore **/*lock.hcl -ignore **/*pb2.py -ignore **/*pb2_grpc.py -ignore **/*.csv -ignore **/.venv/** -ignore **/.idea/** -ignore distros/kubernetes/nvsentinel/charts/mongodb-store/charts/mongodb/Chart.yaml -ignore distros/kubernetes/nvsentinel/charts/mongodb-store/charts/mongodb/charts/common/Chart.yaml -ignore health-monitors/gpu-health-monitor/pyproject.toml -ignore nvsentinel-log-collector/pyproject.toml .

# Check go.mod files for proper replace directives
.PHONY: gomod-lint
gomod-lint:
	@echo "Validating go.mod files for local module replace directives..."
	./scripts/validate-gomod.sh

# Sync dependencies across all go modules using Go workspace
.PHONY: dependencies-sync
dependencies-sync: dependencies-update go-mod-tidy-all
	@echo "go.mod and go.sum updated and synced successfully across all go modules"

# Update dependencies across all go modules using Go workspace
.PHONY: dependencies-update
dependencies-update:
	@echo "Updating dependencies across all Go modules..."
	rm go.work >/dev/null 2>&1 || true
	find . -name "go.mod" | awk -F/go.mod '{print $$1}' | xargs go work init
	go work sync
	rm go.work go.work.sum >/dev/null 2>&1 || true
	@echo "Dependencies updated successfully"

# Sync dependencies and lint to ensure no files were modified
.PHONY: dependencies-sync-lint
dependencies-sync-lint: dependencies-sync
	@echo "Checking if dependency sync modified any files..."
	git status --porcelain --untracked-files=no
	git --no-pager diff
	@echo "Verifying that dependency sync didn't modify any files..."
	test -z "$$(git status --porcelain --untracked-files=no)"

# Run go mod tidy in all directories with go.mod files
.PHONY: go-mod-tidy-all
go-mod-tidy-all:
	@echo "Running go mod tidy in all directories with go.mod files..."
	@find . -name "go.mod" -type f | while read -r gomod_file; do \
		dir=$$(dirname "$$gomod_file"); \
		echo "Running go mod tidy in $$dir..."; \
		(cd "$$dir" && go mod tidy) || exit 1; \
	done
	@echo "go mod tidy completed in all modules"

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

.PHONY: lint-test-platform-connectors
lint-test-platform-connectors:
	@echo "Linting and testing platform-connectors (using standardized Makefile)..."
	$(MAKE) -C platform-connectors lint-test

.PHONY: lint-test-health-events-analyzer
lint-test-health-events-analyzer:
	@echo "Linting and testing health-events-analyzer (using standardized Makefile)..."
	$(MAKE) -C health-events-analyzer lint-test

.PHONY: lint-test-fault-quarantine-module
lint-test-fault-quarantine-module:
	@echo "Linting and testing fault-quarantine-module (using standardized Makefile)..."
	$(MAKE) -C fault-quarantine-module lint-test

.PHONY: lint-test-labeler-module
lint-test-labeler-module:
	@echo "Linting and testing labeler-module (using standardized Makefile)..."
	$(MAKE) -C labeler-module lint-test

.PHONY: lint-test-node-drainer-module
lint-test-node-drainer-module:
	@echo "Linting and testing node-drainer-module (using standardized Makefile)..."
	$(MAKE) -C node-drainer-module lint-test

.PHONY: lint-test-fault-remediation-module
lint-test-fault-remediation-module:
	@echo "Linting and testing fault-remediation-module (using standardized Makefile)..."
	$(MAKE) -C fault-remediation-module lint-test

.PHONY: lint-test-store-client-sdk
lint-test-store-client-sdk:
	@echo "Linting and testing store-client-sdk..."
	$(MAKE) -C store-client-sdk lint-test

.PHONY: lint-test-statemanager
lint-test-statemanager:
	@echo "Linting and testing statemanager..."
	$(MAKE) -C statemanager lint-test

# Python module lint-test targets (non-health-monitors)
# Currently no non-health-monitor Python modules

# Kubernetes distro lint (delegate to distros/kubernetes/Makefile)
.PHONY: kubernetes-distro-lint
kubernetes-distro-lint:
	@echo "Linting Kubernetes distribution..."
	$(MAKE) -C distros/kubernetes lint

# Helm chart validation
.PHONY: helm-lint
helm-lint:
	@echo "🎯 Validating Helm charts..."
	@# Ensure helm is available
	@if ! command -v helm >/dev/null 2>&1; then \
		echo "❌ Error: helm command not found. Please install Helm first."; \
		exit 1; \
	fi
	@echo "Using Helm version: $$(helm version --short)"
	@echo ""
	@# Main nvsentinel chart
	@echo "Validating main nvsentinel chart..."
	helm lint distros/kubernetes/nvsentinel/
	@echo ""
	@# Individual component charts
	@echo "Validating component charts..."
	@for chart_dir in distros/kubernetes/nvsentinel/charts/*/; do \
		if [[ -f "$$chart_dir/Chart.yaml" ]]; then \
			chart_name=$$(basename "$$chart_dir"); \
			echo "Validating chart: $$chart_name"; \
			helm lint "$$chart_dir" -f distros/kubernetes/nvsentinel/values.yaml || exit 1; \
			echo "Testing template rendering for: $$chart_name"; \
			helm template "$$chart_name" "$$chart_dir" -f distros/kubernetes/nvsentinel/values.yaml >/dev/null || exit 1; \
			echo ""; \
		fi; \
	done
	@echo "✅ All Helm charts validated successfully"

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
.PHONY: build-syslog-health-monitor
build-syslog-health-monitor:
	$(MAKE) -C health-monitors build-syslog-health-monitor

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
		$(MAKE) -C $$module clean || exit 1; \
	done

# Docker targets (delegate to docker/Makefile) - standardized build system
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
.PHONY: docker-syslog-health-monitor
docker-syslog-health-monitor:
	$(MAKE) -C docker build-syslog-health-monitor

.PHONY: docker-csp-health-monitor
docker-csp-health-monitor:
	$(MAKE) -C docker build-csp-health-monitor

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
.PHONY: e2e-test-ci
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
	@echo "  install-lint-tools     - Install required lint tools (golangci-lint, gotestsum, etc.)"
	@echo "  install-go-ci          - Install Go $(GO_VERSION) for CI environments (Linux/macOS, amd64/arm64)"
	@echo "  protos-generate        - Generate protobuf files from .proto sources"
	@echo "  protos-lint            - Generate and check protobuf files"
	@echo "  protos-clean           - Remove all generated protobuf files"
	@echo "  license-headers-lint   - Check license headers"
	@echo "  gomod-lint             - Validate go.mod files for local module replace directives"
	@echo "  dependencies-sync      - Sync dependencies across all Go modules using workspace"
	@echo "  dependencies-sync-lint - Sync dependencies and verify no files were modified"
	@echo "  go-mod-tidy-all        - Run go mod tidy in all directories with go.mod files"
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
	@echo "Docker targets (delegated to docker/Makefile) - standardized build system:"
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
	@echo "  docker-syslog-health-monitor    - Build syslog health monitor"
	@echo "  docker-csp-health-monitor       - Build CSP health monitor"
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
	@echo "  - Docker targets use standardized build system"
	@echo "  - Development clusters use ctlptl for declarative management"
	@echo "  - Environment variables: NVCR_CONTAINER_REPO, NGC_ORG, SAFE_REF_NAME, PLATFORMS"
