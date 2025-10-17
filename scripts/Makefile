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

# Find all shell scripts in the current directory
SHELL_SCRIPTS := $(wildcard *.sh)

# Lint shell scripts using shellcheck
.PHONY: lint-shell
lint-shell:
	@echo "Linting shell scripts with shellcheck..."
	@for script in $(SHELL_SCRIPTS); do \
		echo "Checking $$script..."; \
		shellcheck "$$script" || exit 1; \
	done
	@echo "All shell scripts passed shellcheck!"

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
	@echo "Shell scripts (auto-detected): $(SHELL_SCRIPTS)"
	@echo ""
	@echo "Dependencies:"
	@echo "  shellcheck - Required for shell script linting"
	@echo ""
	@echo "Usage:"
	@echo "  make lint         # Lint all shell scripts"
	@echo "  make lint-shell   # Run shellcheck only"
