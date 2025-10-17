# Development environment Makefile
# Contains targets for development environment setup and management

# Configuration variables
CTLPTL_CONFIG_FILE ?= ../ctlptl-config.yaml
CLUSTER_NAME ?= kind-nvsentinel
REGISTRY_NAME ?= ctlptl-registry
REGISTRY_PORT ?= 5001

# Tilt development targets
.PHONY: tilt-up
tilt-up:
	@echo "Starting Tilt development environment..."
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "Error: tilt is not installed. Please install from https://tilt.dev/"; \
		exit 1; \
	fi
	cd .. && tilt up

.PHONY: tilt-down
tilt-down:
	@echo "Stopping Tilt development environment..."
	@if command -v tilt >/dev/null 2>&1; then \
		cd .. && tilt down; \
	else \
		echo "Warning: tilt is not installed"; \
	fi

.PHONY: tilt-ci
tilt-ci:
	@echo "Running Tilt in CI mode (no UI)..."
	@if ! command -v tilt >/dev/null 2>&1; then \
		echo "Error: tilt is not installed. Please install from https://tilt.dev/"; \
		exit 1; \
	fi
	cd .. && tilt ci

# ctlptl cluster configuration
.PHONY: cluster-create
cluster-create:
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
	@echo "Cluster created successfully!"
	@echo "Registry available at localhost:$(REGISTRY_PORT)"

.PHONY: cluster-delete
cluster-delete:
	@echo "Deleting local development cluster..."
	@if [ ! -f "$(CTLPTL_CONFIG_FILE)" ]; then \
		echo "Error: ctlptl config file $(CTLPTL_CONFIG_FILE) not found"; \
		exit 1; \
	fi
	ctlptl delete -f $(CTLPTL_CONFIG_FILE) || echo "Resources from $(CTLPTL_CONFIG_FILE) not found"

.PHONY: cluster-status
cluster-status:
	@echo "Checking cluster status..."
	echo "ctlptl clusters:"; \
	ctlptl get clusters || echo "No ctlptl clusters found"; \
	echo "ctlptl registries:"; \
	ctlptl get registries || echo "No ctlptl registries found"; \

# Combined development environment targets
.PHONY: env-up
env-up: cluster-create tilt-up

.PHONY: env-down
env-down: tilt-down cluster-delete

# Quick development workflow targets
.PHONY: restart
restart: tilt-down tilt-up

.PHONY: reset
reset: env-down env-up

# Help target
.PHONY: help
help:
	@echo "Development Environment Makefile"
	@echo ""
	@echo "Main targets:"
	@echo "  env-up         - Create cluster and start Tilt (full setup)"
	@echo "  env-down       - Stop Tilt and delete cluster (full cleanup)"
	@echo ""
	@echo "Tilt targets:"
	@echo "  tilt-up        - Start Tilt development environment"
	@echo "  tilt-down      - Stop Tilt development environment"
	@echo "  tilt-ci        - Run Tilt in CI mode (no UI)"
	@echo "  restart        - Restart Tilt (tilt-down + tilt-up)"
	@echo ""
	@echo "Cluster targets (using ctlptl):"
	@echo "  cluster-create - Create local ctlptl-managed Kind cluster with registry"
	@echo "  cluster-delete - Delete local ctlptl-managed cluster and registry"
	@echo "  cluster-status - Show cluster and registry status"
	@echo ""
	@echo "Quick workflow targets:"
	@echo "  reset          - Full reset (env-down + env-up)"
	@echo ""
	@echo "Configuration variables:"
	@echo "  CTLPTL_CONFIG_FILE     - ctlptl config file (default: ../ctlptl-config.yaml)"
	@echo "  CLUSTER_NAME           - Cluster name (default: nvsentinel)"
	@echo "  REGISTRY_NAME          - Registry name (default: ctlptl-registry)"
	@echo "  REGISTRY_PORT          - Registry port (default: 5001)"
	@echo ""
	@echo "Notes:"
	@echo "  - ctlptl cluster requires Docker, ctlptl, and Kind to be installed"
	@echo "  - Registry is available at localhost:5001 (or custom port)"
	@echo "  - ctlptl provides declarative cluster management with YAML configuration"
	@echo "  - Install ctlptl: brew install tilt-dev/tap/ctlptl or go install github.com/tilt-dev/ctlptl/cmd/ctlptl@latest"
