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

# Load tool versions from .versions.yaml (single source of truth)
# Requires yq to be installed: brew install yq (macOS) or see https://github.com/mikefarah/yq
YQ := $(shell command -v yq 2> /dev/null)

ifndef YQ
$(warning yq not found - using default versions. Install yq for version management: brew install yq)
GOLANGCI_LINT_VERSION := v1.64.8
GOTESTSUM_VERSION := latest
GOCOVER_COBERTURA_VERSION := latest
GO_VERSION := 1.24
PYTHON_VERSION := 3.11
POETRY_VERSION := 1.8.2
PROTOBUF_VERSION := v27.1
PROTOC_GEN_GO_VERSION := v1.36.6
PROTOC_GEN_GO_GRPC_VERSION := v1.3.0
GRPCIO_TOOLS_VERSION := 1.75.1
BLACK_VERSION:= '25.9.0'
SHELLCHECK_VERSION := v0.11.0
ADDLICENSE_VERSION := latest
DOCKER_BUILDX_VERSION := latest
KO_VERSION := v0.18.0
else
# Load versions from .versions.yaml
ADDLICENSE_VERSION := $(shell $(YQ) '.linting.addlicense' .versions.yaml)
DOCKER_BUILDX_VERSION := $(shell $(YQ) '.container_tools.docker_buildx' .versions.yaml)
GO_VERSION := $(shell $(YQ) '.languages.go' .versions.yaml)
GOCOVER_COBERTURA_VERSION := $(shell $(YQ) '.go_tools.gocover_cobertura' .versions.yaml)
GOLANGCI_LINT_VERSION := $(shell $(YQ) '.go_tools.golangci_lint' .versions.yaml)
GOTESTSUM_VERSION := $(shell $(YQ) '.go_tools.gotestsum' .versions.yaml)
GRPCIO_TOOLS_VERSION := $(shell $(YQ) '.protobuf.grpcio_tools' .versions.yaml)
KO_VERSION := $(shell $(YQ) '.container_tools.ko' .versions.yaml)
POETRY_VERSION := $(shell $(YQ) '.build_tools.poetry' .versions.yaml)
PROTOBUF_VERSION := $(shell $(YQ) '.protobuf.protobuf' .versions.yaml)
PROTOC_GEN_GO_GRPC_VERSION := $(shell $(YQ) '.protobuf.protoc_gen_go_grpc' .versions.yaml)
PROTOC_GEN_GO_VERSION := $(shell $(YQ) '.protobuf.protoc_gen_go' .versions.yaml)
PYTHON_VERSION := $(shell $(YQ) '.languages.python' .versions.yaml)
BLACK_VERSION := $(shell $(YQ) '.linting.black' .versions.yaml)
SHELLCHECK_VERSION := $(shell $(YQ) '.linting.shellcheck' .versions.yaml)
endif

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
	janitor \
	store-client-sdk \
	statemanager \
	commons


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
	fault-remediation-module \
	janitor

# Modules requiring kubebuilder for tests
KUBEBUILDER_MODULES := \
	node-drainer-module \
	fault-remediation-module

# Default target
.PHONY: all
all: lint-test-all ## Run lint-test-all (default target)

# Show loaded tool versions
.PHONY: show-versions
show-versions: ## Display all tool versions loaded from .versions.yaml
	@echo "=== Tool Versions (from .versions.yaml) ==="
	@echo "Languages:"
	@echo "  Go:                     $(GO_VERSION)"
	@echo "  Python:                 $(PYTHON_VERSION)"
	@echo ""
	@echo "Build Tools:"
	@echo "  Poetry:                 $(POETRY_VERSION)"
	@echo ""
	@echo "Go Tools:"
	@echo "  golangci-lint:          $(GOLANGCI_LINT_VERSION)"
	@echo "  gotestsum:              $(GOTESTSUM_VERSION)"
	@echo "  gocover-cobertura:      $(GOCOVER_COBERTURA_VERSION)"
	@echo ""
	@echo "Protocol Buffers:"
	@echo "  protobuf:               $(PROTOBUF_VERSION)"
	@echo "  protoc-gen-go:          $(PROTOC_GEN_GO_VERSION)"
	@echo "  protoc-gen-go-grpc:     $(PROTOC_GEN_GO_GRPC_VERSION)"
	@echo "  grpcio-tools:           $(GRPCIO_TOOLS_VERSION)"
	@echo ""
	@echo "Linting:"
	@echo "  black:                  $(BLACK_VERSION)"
	@echo "  shellcheck:             $(SHELLCHECK_VERSION)"
	@echo "  addlicense:             $(ADDLICENSE_VERSION)"
	@echo ""
	@echo "Container Tools:"
	@echo "  docker-buildx:          $(DOCKER_BUILDX_VERSION)"
	@echo "  ko:                     $(KO_VERSION)"
	@echo "==========================================="
ifndef YQ
	@echo ""
	@echo "⚠️  WARNING: yq not found - using default versions"
	@echo "   Install yq for automatic version loading:"
	@echo "   macOS:  brew install yq"
	@echo "   Linux:  See https://github.com/mikefarah/yq"
endif

# Setup development environment
.PHONY: dev-env-setup
dev-env-setup: ## Setup complete development environment (installs all required tools)
	@echo "Setting up NVSentinel development environment..."
	@bash scripts/setup-dev-env.sh

# Install lint tools
.PHONY: install-lint-tools
install-lint-tools: install-golangci-lint install-gotestsum install-gocover-cobertura ## Install all lint tools (golangci-lint, gotestsum, gocover-cobertura)
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
install-go-ci: ## Install Go $(GO_VERSION) for CI environments (Linux/macOS, amd64/arm64)
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
lint-test-all: protos-lint license-headers-lint gomod-lint health-monitors-lint-test-all go-lint-test-all python-lint-test-all kubernetes-distro-lint log-collector-lint ## Lint and test all modules

# Health monitors lint-test (delegate to health-monitors/Makefile)
.PHONY: health-monitors-lint-test-all
health-monitors-lint-test-all:
	@echo "Running lint and tests for all health monitors..."
	$(MAKE) -C health-monitors lint-test-all

# Generate protobuf files
.PHONY: protos-generate
protos-generate: protos-clean ## Generate protobuf files from .proto sources
	@echo "Generating protobuf files in data-models (Go) and gpu-health-monitor (Python)..."
	@echo "=== Tool Versions ==="
	@echo "Go: $$(go version)"
	@echo "protoc: $$(protoc --version)"
	@echo "protoc-gen-go: $$(protoc-gen-go --version)"
	@echo "protoc-gen-go-grpc: $$(protoc-gen-go-grpc --version)"
	@if command -v python3 >/dev/null 2>&1; then \
		grpcio_tools_version=$$(python3 -c "import importlib.metadata; print('grpcio-tools', importlib.metadata.version('grpcio-tools'))" 2>/dev/null || echo "grpcio-tools: not installed"); \
		echo "$$grpcio_tools_version"; \
	fi
	@if command -v black >/dev/null 2>&1; then \
		black_version=$$(black --version 2>/dev/null | head -1 || echo "black: not available"); \
		echo "$$black_version"; \
	else \
		echo "black: not found"; \
	fi
	@echo "========================"
	# Generate Go protobuf files in data-models (shared by all Go modules)
	$(MAKE) -C data-models protos-generate
	# Generate Python protobuf files for gpu-health-monitor
	$(MAKE) -C health-monitors/gpu-health-monitor protos-generate

# Check protobuf files
.PHONY: protos-lint
protos-lint: protos-generate ## Generate and check protobuf files are up to date
	@echo "Checking protobuf files..."
	git status --porcelain --untracked-files=no
	git --no-pager diff
	@echo "Checking if protobuf files are up to date..."
	test -z "$$(git status --porcelain --untracked-files=no)"

# Clean generated protobuf files
.PHONY: protos-clean
protos-clean: ## Remove all generated protobuf files
	@echo "Cleaning generated protobuf files..."
	@echo "Removing Go protobuf files (.pb.go)..."
	find . -name "*.pb.go" -type f -delete
	@echo "Removing Python protobuf files (*_pb2.py, *_pb2_grpc.py, *_pb2.pyi)..."
	find . \( -name "*_pb2.py" -o -name "*_pb2_grpc.py" -o -name "*_pb2.pyi" \) -type f -delete
	@echo "All generated protobuf files have been removed."

# Check license headers
.PHONY: license-headers-lint
license-headers-lint: ## Check license headers in source files
	@echo "Checking license headers..."
	addlicense -f .github/headers/LICENSE -check \
		-ignore '.venv/**' \
		-ignore '**/__pycache__/**' \
		-ignore '**/.venv/**' \
		-ignore '**/site-packages/**' \
		-ignore '*/.venv/**' \
		-ignore '**/.idea/**' \
		-ignore '**/*.csv' \
		-ignore '**/*.pyc' \
		-ignore '**/*.txt' \
		-ignore '**/*.xml' \
		-ignore '**/*.yaml' \
		-ignore '**/*.toml' \
		-ignore '**/*lock.hcl' \
		-ignore '**/*pb2*' \
		.

# Check go.mod files for proper replace directives
.PHONY: gomod-lint
gomod-lint: ## Validate go.mod files for local module replace directives
	@echo "Validating go.mod files for local module replace directives..."
	./scripts/validate-gomod.sh

# Sync dependencies across all go modules using Go workspace
.PHONY: dependencies-sync
dependencies-sync: dependencies-update go-mod-tidy-all ## Sync dependencies across all Go modules using workspace
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
dependencies-sync-lint: dependencies-sync ## Sync dependencies and verify no files were modified
	@echo "Checking if dependency sync modified any files..."
	git status --porcelain --untracked-files=no
	git --no-pager diff
	@echo "Verifying that dependency sync didn't modify any files..."
	test -z "$$(git status --porcelain --untracked-files=no)"

# Run go mod tidy in all directories with go.mod files
.PHONY: go-mod-tidy-all
go-mod-tidy-all: ## Run go mod tidy in all directories with go.mod files
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

.PHONY: lint-test-janitor
lint-test-janitor:
	@echo "Linting and testing janitor (using standardized Makefile)..."
	$(MAKE) -C janitor lint-test

.PHONY: lint-test-store-client-sdk
lint-test-store-client-sdk:
	@echo "Linting and testing store-client-sdk..."
	$(MAKE) -C store-client-sdk lint-test

.PHONY: lint-test-statemanager
lint-test-statemanager:
	@echo "Linting and testing statemanager..."
	$(MAKE) -C statemanager lint-test

.PHONY: lint-test-commons
lint-test-commons:
	@echo "Linting and testing commons..."
	$(MAKE) -C commons lint-test

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
log-collector-lint: ## Lint shell scripts in log collector
	@echo "Linting log collector shell scripts..."
	$(MAKE) -C nvsentinel-log-collector lint

# Build targets (delegate to sub-Makefiles for better organization)
.PHONY: build-all
build-all: build-health-monitors build-main-modules ## Build all modules

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
clean-all: clean-health-monitors clean-main-modules ## Clean all modules

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
docker-all: ## Build all Docker images
	@echo "Building all Docker images..."
	$(MAKE) -C docker build-all

.PHONY: docker-publish-all
docker-publish-all: ## Build and publish all Docker images
	@echo "Building and publishing all Docker images..."
	$(MAKE) -C docker publish-all

.PHONY: docker-setup-buildx
docker-setup-buildx: ## Setup Docker buildx builder
	$(MAKE) -C docker setup-buildx

# Ko targets - build Go container images without Docker
.PHONY: ko-build
ko-build: ## Build all ko-based container images locally
	@echo "Building all ko-based container images..."
	@./scripts/buildko.sh

.PHONY: ko-publish
ko-publish: ## Build and publish all ko-based container images
	@echo "Building and publishing all ko-based container images..."
	@./scripts/buildko.sh

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

.PHONY: docker-janitor
docker-janitor:
	$(MAKE) -C docker build-janitor

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

# ============================================================================
# Development Environment Targets
# ============================================================================
# Configuration variables
CTLPTL_CONFIG_FILE ?= ctlptl-config.yaml
CLUSTER_NAME ?= kind-nvsentinel
REGISTRY_NAME ?= ctlptl-registry
REGISTRY_PORT ?= 5001

# Tilt development targets
.PHONY: tilt-up
tilt-up: ## Start Tilt development environment
	@echo "Starting Tilt development environment..."
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "Error: tilt is not installed. Please install from https://tilt.dev/"; \
		exit 1; \
	fi
	tilt up -f tilt/Tiltfile

.PHONY: tilt-down
tilt-down: ## Stop Tilt development environment
	@echo "Stopping Tilt development environment..."
	@if command -v tilt >/dev/null 2>&1; then \
		tilt down -f tilt/Tiltfile; \
	else \
		echo "Warning: tilt is not installed"; \
	fi

.PHONY: tilt-ci
tilt-ci: ## Run Tilt in CI mode (no UI, waits for all resources)
	@echo "Running Tilt in CI mode (no UI)..."
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "Error: tilt is not installed. Please install from https://tilt.dev/"; \
		exit 1; \
	fi
	tilt ci -f tilt/Tiltfile
	@echo "Waiting for all deployments to be ready..."
	@kubectl get deployments --all-namespaces --no-headers -o custom-columns=":metadata.namespace,:metadata.name" | while read ns name; do \
		echo "Waiting for deployment $$name in namespace $$ns..."; \
		kubectl rollout status deployment/$$name -n $$ns --timeout=300s || exit 1; \
	done
	@echo "Waiting for all daemonsets to be ready..."
	@kubectl get daemonsets --all-namespaces --no-headers -o custom-columns=":metadata.namespace,:metadata.name" | while read ns name; do \
		echo "Waiting for daemonset $$name in namespace $$ns..."; \
		kubectl rollout status daemonset/$$name -n $$ns --timeout=300s || exit 1; \
	done
	@echo "Waiting for all statefulsets to be ready..."
	@kubectl get statefulsets --all-namespaces --no-headers -o custom-columns=":metadata.namespace,:metadata.name" | while read ns name; do \
		echo "Waiting for statefulset $$name in namespace $$ns..."; \
		kubectl rollout status statefulset/$$name -n $$ns --timeout=300s || exit 1; \
	done
	@echo "All workloads are ready!"

# ctlptl cluster configuration
.PHONY: cluster-create
cluster-create: ## Create local ctlptl-managed Kind cluster with registry
	@echo "Creating local development cluster with ctlptl..."
	@if ! command -v ctlptl >/dev/null 2>&1; then \
		echo "Error: ctlptl is not installed. Please install from https://github.com/tilt-dev/ctlptl"; \
		echo "Installation options:"; \
		echo "  - Homebrew: brew install tilt-dev/tap/ctlptl"; \
		echo "  - Go: go install github.com/tilt-dev/ctlptl/cmd/ctlptl@latest"; \
		exit 1; \
	fi
	@if ! command -v docker >/dev/null 2>&1; then \
		echo "Error: docker is not installed. Please install Docker"; \
		exit 1; \
	fi
	@if ! command -v kind >/dev/null 2>&1; then \
		echo "Error: kind is not installed. Please install from https://kind.sigs.k8s.io/"; \
		exit 1; \
	fi
	@echo "Creating cluster and registry with ctlptl..."
	@if [ ! -f "$(CTLPTL_CONFIG_FILE)" ]; then \
		echo "Error: ctlptl config file $(CTLPTL_CONFIG_FILE) not found"; \
		exit 1; \
	fi
	ctlptl apply -f $(CTLPTL_CONFIG_FILE)
	@echo "Waiting for all nodes to be ready..."
	@kubectl wait --for=condition=ready nodes --all --timeout=300s
	@echo "Cluster created successfully!"
	@echo "Registry available at localhost:$(REGISTRY_PORT)"

.PHONY: cluster-delete
cluster-delete: ## Delete local ctlptl-managed cluster and registry
	@echo "Deleting local development cluster..."
	@if [ ! -f "$(CTLPTL_CONFIG_FILE)" ]; then \
		echo "Error: ctlptl config file $(CTLPTL_CONFIG_FILE) not found"; \
		exit 1; \
	fi
	ctlptl delete -f $(CTLPTL_CONFIG_FILE) || echo "Resources from $(CTLPTL_CONFIG_FILE) not found"

.PHONY: cluster-status
cluster-status: ## Show cluster and registry status
	@echo "=== Cluster Status ==="
	@if command -v ctlptl >/dev/null 2>&1; then \
		echo "ctlptl clusters:"; \
		ctlptl get clusters 2>/dev/null || echo "No ctlptl clusters found"; \
		echo ""; \
	fi
	@if command -v kubectl >/dev/null 2>&1 && kubectl cluster-info >/dev/null 2>&1; then \
		echo "Current kubectl context: $$(kubectl config current-context)"; \
		echo ""; \
		echo "Cluster nodes:"; \
		kubectl get nodes -o wide 2>/dev/null || echo "Unable to get nodes"; \
		echo ""; \
		echo "Registry status:"; \
		docker ps --filter "name=$(REGISTRY_NAME)" --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}" 2>/dev/null || echo "Registry container not found"; \
	else \
		echo "No active cluster (kubectl not configured or cluster not accessible)"; \
	fi
	@echo "======================"

# Combined development environment targets
.PHONY: dev-env
dev-env: cluster-create tilt-up ## Create cluster and start Tilt (full development setup)

.PHONY: dev-env-clean
dev-env-clean: tilt-down cluster-delete ## Stop Tilt and delete cluster (full cleanup)

# Quick development workflow targets
.PHONY: dev-restart
dev-restart: tilt-down tilt-up ## Restart Tilt without recreating cluster

.PHONY: dev-reset
dev-reset: dev-env-clean dev-env ## Full reset (tear down and recreate everything)

# Tilt end-to-end test target for CI
.PHONY: e2e-test-ci
e2e-test-ci: tilt-ci ## Run end-to-end test suite in CI mode
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
list-modules: ## List all modules (Go, Python, container-only)
	@echo "Go modules:"
	@for module in $(GO_MODULES); do echo "  $$module"; done
	@echo "Python modules:"
	@for module in $(PYTHON_MODULES); do echo "  $$module"; done
	@echo "Container-only modules:"
	@for module in $(CONTAINER_MODULES); do echo "  $$module"; done

.PHONY: help
help: ## Display available make targets
	@echo "nvsentinel Main Makefile"
	@echo ""
	@echo "Available targets:"
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort -u | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-30s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Configuration variables:"
	@echo "  GO_VERSION=$(GO_VERSION), GOLANGCI_LINT_VERSION=$(GOLANGCI_LINT_VERSION)"
	@echo "  CTLPTL_CONFIG_FILE=$(CTLPTL_CONFIG_FILE), REGISTRY_PORT=$(REGISTRY_PORT)"
	@echo ""
	@echo "Sub-Makefiles: health-monitors/, docker/, distros/kubernetes/, tests/"
