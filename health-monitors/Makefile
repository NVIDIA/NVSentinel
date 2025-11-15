# Health monitors Makefile
# Contains targets for all health monitoring modules

# Go binary and tools
GO := go
GOLANGCI_LINT := golangci-lint
GOTESTSUM := gotestsum
GOCOVER_COBERTURA := gocover-cobertura

# Health monitor modules
GO_HEALTH_MONITORS := \
	syslog-health-monitor \
	csp-health-monitor \
	kubernetes-object-monitor

PYTHON_HEALTH_MONITORS := \
	gpu-health-monitor

# Private modules requiring special authentication
PRIVATE_HEALTH_MONITORS := \
	csp-health-monitor

# Lint and test all health monitors
.PHONY: lint-test-all
lint-test-all: go-lint-test-all python-lint-test-all

# Lint and test all Go health monitors
.PHONY: go-lint-test-all
go-lint-test-all:
	@echo "Running lint and tests for all Go health monitors..."
	@for module in $(GO_HEALTH_MONITORS); do \
		echo "Processing $$module..."; \
		$(MAKE) lint-test-$$module || exit 1; \
	done

# Lint and test all Python health monitors
.PHONY: python-lint-test-all
python-lint-test-all:
	@echo "Running lint and tests for all Python health monitors..."
	@for module in $(PYTHON_HEALTH_MONITORS); do \
		echo "Processing $$module..."; \
		$(MAKE) lint-test-$$module || exit 1; \
	done

# Individual health monitor lint-test targets (delegate to module Makefiles)
.PHONY: lint-test-syslog-health-monitor
lint-test-syslog-health-monitor:
	$(MAKE) -C syslog-health-monitor lint-test

.PHONY: lint-test-csp-health-monitor
lint-test-csp-health-monitor:
	$(MAKE) -C csp-health-monitor lint-test

.PHONY: lint-test-gpu-health-monitor
lint-test-gpu-health-monitor:
	$(MAKE) -C gpu-health-monitor lint-test

.PHONY: lint-test-kubernetes-object-monitor
lint-test-kubernetes-object-monitor:
	$(MAKE) -C kubernetes-object-monitor lint-test

# Build targets for health monitors (delegate to module Makefiles)
.PHONY: build-all
build-all:
	@echo "Building all health monitors..."
	@for module in $(GO_HEALTH_MONITORS); do \
		echo "Building $$module..."; \
		$(MAKE) -C $$module build || exit 1; \
	done
	@echo "Building gpu-health-monitor (Python module)..."
	$(MAKE) -C gpu-health-monitor setup

# Individual build targets (delegate to module Makefiles)
.PHONY: build-syslog-health-monitor
build-syslog-health-monitor:
	$(MAKE) -C syslog-health-monitor build

.PHONY: build-csp-health-monitor
build-csp-health-monitor:
	$(MAKE) -C csp-health-monitor build

.PHONY: build-gpu-health-monitor
build-gpu-health-monitor:
	$(MAKE) -C gpu-health-monitor setup

.PHONY: build-kubernetes-object-monitor
build-kubernetes-object-monitor:
	$(MAKE) -C kubernetes-object-monitor build

# Clean targets (delegate to module Makefiles)
.PHONY: clean-all
clean-all:
	@echo "Cleaning all health monitors..."
	@for module in $(GO_HEALTH_MONITORS); do \
		echo "Cleaning $$module..."; \
		$(MAKE) -C $$module clean || exit 1; \
	done
	@echo "Cleaning gpu-health-monitor..."
	$(MAKE) -C gpu-health-monitor clean

# Individual clean targets
.PHONY: clean-syslog-health-monitor
clean-syslog-health-monitor:
	$(MAKE) -C syslog-health-monitor clean

.PHONY: clean-csp-health-monitor
clean-csp-health-monitor:
	$(MAKE) -C csp-health-monitor clean

.PHONY: clean-gpu-health-monitor
clean-gpu-health-monitor:
	$(MAKE) -C gpu-health-monitor clean

.PHONY: clean-kubernetes-object-monitor
clean-kubernetes-object-monitor:
	$(MAKE) -C kubernetes-object-monitor clean

# Help target
.PHONY: help
help:
	@echo "Health Monitors Makefile - coordinates individual health monitor modules"
	@echo ""
	@echo "Main targets:"
	@echo "  lint-test-all          - Lint and test all health monitors"
	@echo "  go-lint-test-all       - Lint and test all Go health monitors"
	@echo "  python-lint-test-all   - Lint and test all Python health monitors"
	@echo ""
	@echo "Individual lint-test targets (delegated to module Makefiles):"
	@for module in $(GO_HEALTH_MONITORS) $(PYTHON_HEALTH_MONITORS); do echo "  lint-test-$$module"; done
	@echo ""
	@echo "Build targets (delegated to module Makefiles):"
	@echo "  build-all              - Build all health monitors"
	@for module in $(GO_HEALTH_MONITORS) $(PYTHON_HEALTH_MONITORS); do echo "  build-$$module"; done
	@echo ""
	@echo "Clean targets (delegated to module Makefiles):"
	@echo "  clean-all              - Clean all health monitors"
	@for module in $(GO_HEALTH_MONITORS) $(PYTHON_HEALTH_MONITORS); do echo "  clean-$$module"; done
	@echo ""
	@echo "Individual module Makefiles:"
	@echo "  Each health monitor has its own Makefile with additional targets:"
	@for module in $(GO_HEALTH_MONITORS) $(PYTHON_HEALTH_MONITORS); do echo "  $$module/Makefile"; done
	@echo ""
	@echo "Usage examples:"
	@echo "  make -C gpu-health-monitor help     - Show GPU health monitor specific targets"
	@echo ""
	@echo "Utility targets:"
	@echo "  help                   - Show this help message"
	@echo ""
	@echo "Notes:"
	@echo "  - Each module has its own Makefile with module-specific targets"
	@echo "  - GPU health monitor is a Python module using Poetry"
