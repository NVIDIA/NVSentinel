# System Services Monitor Makefile (STUB — real impl lands in PR #3)
# Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
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

# =============================================================================
# STUB MAKEFILE
# =============================================================================
# This is the CI-wiring half of the system-services-monitor split (#891 of 5).
# It registers the matrix entry in container-build-test.yml so that the
# follow-up implementation PR's first CI run lands on the real matrix row
# rather than being backfilled afterwards.
#
# The real Makefile (with make/python.mk + make/docker.mk includes, Poetry
# setup, lint/test/docker-publish targets, etc.) lands together with the
# Python implementation in PR #3.

.PHONY: all lint-test docker-build docker-publish help

all: lint-test

lint-test:
	@echo "system-services-monitor: lint-test stub — real impl in PR #3 of #891 split"

docker-build:
	@echo "system-services-monitor: docker-build stub — real impl in PR #3 of #891 split"

docker-publish:
	@echo "system-services-monitor: docker-publish stub — real impl in PR #3 of #891 split"

help:
	@echo "system-services-monitor — STUB Makefile"
	@echo ""
	@echo "This Makefile contains stub targets only. The real targets land"
	@echo "together with the Python implementation in PR #3 of the #891 split."
	@echo ""
	@echo "Available stub targets:"
	@echo "  lint-test       - returns 0 (stub)"
	@echo "  docker-build    - returns 0 (stub)"
	@echo "  docker-publish  - returns 0 (stub)"
