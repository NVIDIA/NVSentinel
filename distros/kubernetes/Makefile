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
	@if [ -z "$$SAFE_REF" ]; then \
		echo "Warning: SAFE_REF not set, skipping publish"; \
	else \
		echo "Updating chart files for release $$SAFE_REF..."; \
		SED_CMD="sed"; \
		if command -v gsed >/dev/null 2>&1; then \
			SED_CMD="gsed"; \
			echo "Using GNU sed (gsed) for compatibility"; \
		fi; \
		$$SED_CMD -i.bak "s|tag: \"main\"|tag: \"$$SAFE_REF\"|" nvsentinel/values.yaml && \
		$$SED_CMD -i.bak "s|appVersion: \"1.0.0\"|appVersion: \"$$SAFE_REF\"|" nvsentinel/Chart.yaml && \
		echo "Updated global.image.tag to $$SAFE_REF in values.yaml"; \
		echo "Updated appVersion to $$SAFE_REF in Chart.yaml"; \
		HELM_OCI_REPO_LOWER=$$(echo "$$HELM_OCI_REPOSITORY" | tr '[:upper:]' '[:lower:]'); \
		echo "Publishing to OCI registry: $$HELM_OCI_REGISTRY/$$HELM_OCI_REPO_LOWER"; \
		helm package nvsentinel --version $$SAFE_REF && \
		helm push nvsentinel-$$SAFE_REF.tgz oci://$$HELM_OCI_REGISTRY/$$HELM_OCI_REPO_LOWER; \
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
	@echo "  - helm-publish requires SAFE_REF environment variable"
	@echo "  - helm-publish automatically updates image tags from 'main' to SAFE_REF"
	@echo "  - helm-publish automatically updates Chart.yaml appVersion to SAFE_REF"
	@echo "  - helm-publish uses gsed (GNU sed) when available, falls back to sed"
	@echo "  - For OCI registry (ghcr.io): set HELM_REGISTRY_TYPE=oci, HELM_OCI_REGISTRY, HELM_OCI_REPOSITORY"
	@echo "  - For traditional registry (default): requires NGC_USER_NAME, NGC_PASSWORD"
	@echo "  - Dependencies are automatically updated during packaging"
