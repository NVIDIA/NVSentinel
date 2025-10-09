# State Manager Makefile
# Individual module build and test targets

# Go binary and tools
GO := go
GOLANGCI_LINT := golangci-lint
GOTESTSUM := gotestsum
GOCOVER_COBERTURA := gocover-cobertura

# Default target
.PHONY: all
all: lint-test

# Lint and test this module (matches parent Makefile expectations)
.PHONY: lint-test
lint-test:
	@echo "Linting and testing statemanager..."
	$(GO) vet ./... && \
	$(GOLANGCI_LINT) run --config ../.golangci.yml && \
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic && \
	$(GO) tool cover -func coverage.txt && \
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# Individual steps
.PHONY: vet
vet:
	@echo "Running go vet..."
	$(GO) vet ./...

.PHONY: lint
lint:
	@echo "Running golangci-lint..."
	$(GOLANGCI_LINT) run --config ../.golangci.yml

.PHONY: test
test:
	@echo "Running tests..."
	$(GOTESTSUM) --junitfile report.xml -- -race $$(go list ./...) -coverprofile=coverage.txt -covermode atomic

.PHONY: coverage
coverage: test
	@echo "Generating coverage reports..."
	$(GO) tool cover -func coverage.txt
	$(GOCOVER_COBERTURA) < coverage.txt > coverage.xml

# Build targets
.PHONY: build
build:
	@echo "Building statemanager..."
	$(GO) build ./...

# Clean target
.PHONY: clean
clean:
	@echo "Cleaning statemanager..."
	$(GO) clean ./...
	rm -f coverage.txt coverage.xml report.xml

# Help target
.PHONY: help
help:
	@echo "State Manager Makefile - Matching .gitlab-ci.yml exactly"
	@echo ""
	@echo "Available targets:"
	@echo "  lint-test     - Run full lint and test suite (matches CI)"
	@echo "  vet          - Run go vet only"
	@echo "  lint         - Run golangci-lint only"
	@echo "  test         - Run tests only"
	@echo "  coverage     - Generate coverage reports"
	@echo "  build        - Build the module"
	@echo "  clean        - Clean build artifacts"
	@echo "  help         - Show this help message"
