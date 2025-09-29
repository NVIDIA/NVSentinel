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
	@if [ -z "$$SAFE_REF_NAME" ]; then \
		echo "Warning: SAFE_REF_NAME not set, skipping publish"; \
	else \
		helm repo add --force-update ngc https://helm.ngc.nvidia.com/nv-ngc-devops --username=$$NGC_USER_NAME --password=$$NGC_PASSWORD && \
		helm package nvsentinel --version $$SAFE_REF_NAME && \
		helm cm-push -f nvsentinel-$$SAFE_REF_NAME.tgz ngc; \
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