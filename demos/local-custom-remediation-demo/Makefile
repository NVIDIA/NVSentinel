# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

.PHONY: help
help:
	@echo "NVSentinel Custom Remediation Demo"
	@echo ""
	@echo "Available targets:"
	@echo "  setup               - Create cluster and install all components"
	@echo "  show-cluster        - Display cluster status and components"
	@echo "  trigger             - Deploy memory hog to trigger remediation"
	@echo "  verify              - Verify the remediation workflow completed"
	@echo "  cleanup             - Delete cluster and clean up"
	@echo "  demo                - Run full automated demo (setup + trigger + verify)"
	@echo ""
	@echo "Step-by-step workflow:"
	@echo "  1. make setup"
	@echo "  2. make show-cluster"
	@echo "  3. make trigger"
	@echo "  4. make verify"
	@echo "  5. make cleanup"

.PHONY: setup
setup:
	@./scripts/00-setup.sh

.PHONY: show-cluster
show-cluster:
	@./scripts/01-show-cluster.sh

.PHONY: trigger
trigger:
	@./scripts/02-trigger-memory-pressure.sh

.PHONY: verify
verify:
	@./scripts/03-verify-remediation.sh

.PHONY: cleanup
cleanup:
	@./scripts/99-cleanup.sh

.PHONY: demo
demo: setup
	@echo ""
	@echo "Waiting 10 seconds for components to stabilize..."
	@sleep 10
	@$(MAKE) show-cluster
	@echo ""
	@echo "Waiting 5 seconds before triggering memory pressure..."
	@sleep 5
	@$(MAKE) trigger
	@echo ""
	@echo "Waiting 60 seconds for remediation workflow to complete..."
	@sleep 60
	@$(MAKE) verify
	@echo ""
	@echo "✅ Demo complete! Run 'make cleanup' to clean up."
