# Scripts Makefile
# Shell script linting and validation

# Default target
.PHONY: all
all: lint

# Lint and test this module (matches parent Makefile expectations)
.PHONY: lint-test
lint-test: lint

# Lint shell scripts
.PHONY: lint
lint: lint-shell

# Lint shell scripts using shellcheck
.PHONY: lint-shell
lint-shell:
	@echo "Linting shell scripts..."
	shellcheck validate-gomod.sh
	shellcheck configure-ctlptl-registry.sh
	shellcheck replace-docker-registry.sh
	shellcheck replace-imports.sh

# Format shell scripts (placeholder for future enhancements)
.PHONY: format
format:
	@echo "Shell script formatting not implemented"
	@echo "Consider using shfmt for shell script formatting"

# Clean target
.PHONY: clean
clean:
	@echo "Cleaning scripts..."
	@echo "No build artifacts to clean for shell scripts"

# Help target
.PHONY: help
help:
	@echo "Scripts Makefile - Shell script linting and validation"
	@echo ""
	@echo "Main targets:"
	@echo "  all        - Run lint (default)"
	@echo "  lint-test  - Run all linting (matches CI expectations)"
	@echo "  lint       - Run shell script linting"
	@echo "  lint-shell - Run shellcheck on all shell scripts"
	@echo "  format     - Format shell scripts (placeholder)"
	@echo "  clean      - Clean build artifacts (no-op for scripts)"
	@echo "  help       - Show this help message"
	@echo ""
	@echo "Shell scripts:"
	@echo "  validate-gomod.sh           - Validates go.mod files and replace directives"
	@echo "  configure-ctlptl-registry.sh - Configures ctlptl registry authentication"
	@echo "  replace-docker-registry.sh  - Replaces Docker registry references"
	@echo "  replace-imports.sh          - Replaces GitLab imports with GitHub imports"
	@echo ""
	@echo "Dependencies:"
	@echo "  shellcheck - Required for shell script linting"
	@echo ""
	@echo "Usage:"
	@echo "  make lint         # Lint all shell scripts"
	@echo "  make lint-shell   # Run shellcheck only"