# Kubernetes distribution Makefile
# Contains targets for Kubernetes/Helm related operations

# Lint Kubernetes/Helm charts
.PHONY: lint
lint:
	@echo "Linting Kubernetes distribution..."
	helm lint nvsentinel/

# Publish Helm chart
.PHONY: helm-publish
helm-publish:
	@echo "Publishing Kubernetes Helm chart..."
	@if [ -z "$$CI_COMMIT_TAG" ]; then \
		echo "Warning: CI_COMMIT_TAG not set, skipping publish"; \
	else \
		helm repo add --force-update ngc https://helm.ngc.nvidia.com/nv-ngc-devops --username=$$NGC_USER_NAME --password=$$NGC_PASSWORD && \
		helm cm-push -f nvsentinel-$$CI_COMMIT_TAG.tgz ngc; \
	fi

# All Kubernetes/Helm operations
.PHONY: all
all: lint

# Help target
.PHONY: help
help:
	@echo "Kubernetes Distribution Makefile"
	@echo ""
	@echo "Main targets:"
	@echo "  all           - Run lint (default)"
	@echo "  lint          - Lint Kubernetes Helm charts"
	@echo "  helm-publish  - Publish Helm chart (requires CI_COMMIT_TAG)"
	@echo "  help          - Show this help message"
	@echo ""
	@echo "Notes:"
	@echo "  - helm-publish requires CI_COMMIT_TAG environment variable"
	@echo "  - Dependencies are automatically updated during packaging"