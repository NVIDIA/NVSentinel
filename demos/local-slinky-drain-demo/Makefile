# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

.PHONY: help
help:
	@echo "NVSentinel Slinky Drain Demo"
	@echo ""
	@echo "Available targets:"
	@echo "  setup               - Create cluster and install all components"
	@echo "  show-cluster        - Display cluster status and components"
	@echo "  inject-health-event - Inject health event to trigger drain"
	@echo "  verify-drain        - Verify the drain workflow completed"
	@echo "  cleanup             - Delete cluster and clean up"
	@echo "  demo                - Run full automated demo (setup + inject + verify)"
	@echo ""
	@echo "Step-by-step workflow:"
	@echo "  1. make setup"
	@echo "  2. make show-cluster"
	@echo "  3. make inject-health-event"
	@echo "  4. make verify-drain"
	@echo "  5. make cleanup"

.PHONY: setup
setup:
	@./scripts/00-setup.sh

.PHONY: show-cluster
show-cluster:
	@./scripts/01-show-cluster.sh

.PHONY: inject-health-event
inject-health-event:
	@./scripts/02-inject-health-event.sh

.PHONY: verify-drain
verify-drain:
	@./scripts/03-verify-drain.sh

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
	@echo "Waiting 5 seconds before injecting health event..."
	@sleep 5
	@$(MAKE) inject-health-event
	@echo ""
	@echo "Waiting 30 seconds for drain workflow to complete..."
	@sleep 30
	@$(MAKE) verify-drain
	@echo ""
	@echo "✅ Demo complete! Run 'make cleanup' to clean up."
