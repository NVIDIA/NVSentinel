# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

.PHONY: help setup show-cluster inject-error verify cleanup demo

help: ## Show this help message
	@echo "NVSentinel Local Demo - Fault Injection"
	@echo ""
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'
	@echo ""
	@echo "Quick start:"
	@echo "  make demo    # Run the complete demo"
	@echo "  make cleanup # Clean up when done"

setup: ## Create KIND cluster and install NVSentinel
	@echo "🚀 Setting up demo environment..."
	@./scripts/00-setup.sh

show-cluster: ## Show current cluster status
	@./scripts/01-show-cluster.sh

inject-error: ## Inject error to simulate GPU failure
	@./scripts/02-inject-error.sh

verify: ## Verify node was cordoned
	@./scripts/03-verify-cordon.sh

cleanup: ## Delete the demo cluster
	@./scripts/99-cleanup.sh

demo: setup show-cluster inject-error verify ## Run the complete demo (setup → show → inject → verify)
	@echo ""
	@echo "✅ Demo complete! Run 'make cleanup' when finished."
	@echo ""

.DEFAULT_GOAL := help



