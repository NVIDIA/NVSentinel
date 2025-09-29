# GPU Health Monitor Makefile
# Individual module build and test targets (Python module)

# Default target
.PHONY: all
all: lint-test

# Lint and test this module (matches parent Makefile expectations)
.PHONY: lint-test
lint-test:
	@echo "Linting and testing gpu-health-monitor..."
	poetry config virtualenvs.in-project true && \
	poetry install && \
	poetry run black --check . && \
	poetry run coverage run --source=gpu_health_monitor -m pytest -vv --junitxml=report.xml && \
	(poetry run coverage xml || true) && \
	(poetry run coverage report || true)

# Individual steps
.PHONY: setup
setup:
	@echo "Setting up Poetry environment..."
	poetry config virtualenvs.in-project true
	poetry install

.PHONY: lint
lint:
	@echo "Running Black formatter check..."
	poetry run black --check .

.PHONY: format
format:
	@echo "Running Black formatter..."
	poetry run black .

.PHONY: test
test:
	@echo "Running tests..."
	poetry run pytest -vv --junitxml=report.xml

.PHONY: test-coverage
test-coverage:
	@echo "Running tests with coverage..."
	poetry run coverage run --source=gpu_health_monitor -m pytest -vv --junitxml=report.xml
	poetry run coverage report
	poetry run coverage xml || true

# Docker configuration matching GitLab CI
NVCR_CONTAINER_REPO ?= nvcr.io
NGC_ORG ?= nv-ngc-devops
CI_COMMIT_REF_NAME ?= $(shell git rev-parse --abbrev-ref HEAD)
SAFE_REF_NAME ?= $(shell echo $(CI_COMMIT_REF_NAME) | sed 's/\//-/g')
SAFE_REF_NAME := $(if $(SAFE_REF_NAME),$(SAFE_REF_NAME),local)
BUILDX_BUILDER ?= nvsentinel-builder
PLATFORMS ?= linux/arm64,linux/amd64

# Setup buildx builder (matching GitLab CI setup)
.PHONY: setup-buildx
setup-buildx:
	@echo "Setting up Docker buildx builder..."
	@docker buildx inspect $(BUILDX_BUILDER) >/dev/null 2>&1 || \
		(docker context create $(BUILDX_BUILDER)-context || true && \
		 docker buildx create --name $(BUILDX_BUILDER) $(BUILDX_BUILDER)-context --driver docker-container --driver-opt network=host --buildkitd-flags '--allow-insecure-entitlement network.host' && \
		 docker buildx use $(BUILDX_BUILDER) && \
		 docker buildx inspect --bootstrap)

# Docker targets with clear separation of local vs CI builds

# Local development builds
.PHONY: docker-build-dcgm3
docker-build-dcgm3: setup-buildx
	@echo "Building Docker image for gpu-health-monitor with DCGM 3.x (local development)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-3 \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-3,mode=max \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-3.x \
		--build-arg DCGM_VERSION=3.3.7-1-ubuntu22.04 \
		--build-arg PYTHONPATH=/usr/local/dcgm/bindings/python3 \
		-f Dockerfile \
		.

.PHONY: docker-build-dcgm4
docker-build-dcgm4: setup-buildx
	@echo "Building Docker image for gpu-health-monitor with DCGM 4.x (local development)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-4 \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-4,mode=max \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-4.x \
		-f Dockerfile \
		.

.PHONY: docker-build
docker-build: docker-build-dcgm3 docker-build-dcgm4
	@echo "Built both DCGM 3.x and 4.x images locally"

# CI/Production builds (--push directly to registry, matches GitLab CI exactly)
.PHONY: docker-publish-dcgm3
docker-publish-dcgm3: setup-buildx
	@echo "Building and publishing Docker image for gpu-health-monitor with DCGM 3.x (CI/production)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-3 \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-3,mode=max \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-3.x \
		--build-arg DCGM_VERSION=3.3.7-1-ubuntu22.04 \
		--build-arg PYTHONPATH=/usr/local/dcgm/bindings/python3 \
		-f Dockerfile \
		.

.PHONY: docker-publish-dcgm4
docker-publish-dcgm4: setup-buildx
	@echo "Building and publishing Docker image for gpu-health-monitor with DCGM 4.x (CI/production)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		--network=host \
		--cache-from=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-4 \
		--cache-to=type=registry,ref=$(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-buildcache:gpu-health-monitor-4,mode=max \
		--push \
		-t $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-4.x \
		-f Dockerfile \
		.

.PHONY: docker-publish
docker-publish: docker-publish-dcgm3 docker-publish-dcgm4
	@echo "Published both DCGM 3.x and 4.x images to registry"

# Legacy targets for backwards compatibility
.PHONY: image-dcgm3
image-dcgm3: docker-build-dcgm3
	@echo "Built DCGM 3.x image locally (legacy target)"

.PHONY: image-dcgm4
image-dcgm4: docker-build-dcgm4
	@echo "Built DCGM 4.x image locally (legacy target)"

.PHONY: image
image: docker-build
	@echo "Built both DCGM 3.x and 4.x images locally (legacy target)"

.PHONY: publish-dcgm3
publish-dcgm3: docker-build-dcgm3
	@echo "Publishing DCGM 3.x image to registry (legacy target)..."
	docker push $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-3.x

.PHONY: publish-dcgm4
publish-dcgm4: docker-build-dcgm4
	@echo "Publishing DCGM 4.x image to registry (legacy target)..."
	docker push $(NVCR_CONTAINER_REPO)/$(NGC_ORG)/nvsentinel-gpu-health-monitor:$(SAFE_REF_NAME)-dcgm-4.x

.PHONY: publish
publish: publish-dcgm3 publish-dcgm4
	@echo "Published both DCGM 3.x and 4.x images (legacy target)"

# Clean target
.PHONY: clean
clean:
	@echo "Cleaning gpu-health-monitor..."
	rm -f report.xml coverage.xml .coverage
	rm -rf .pytest_cache
	rm -rf gpu_health_monitor.egg-info
	find . -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete

# Development targets
.PHONY: shell
shell:
	@echo "Opening Poetry shell..."
	poetry shell

.PHONY: deps
deps:
	@echo "Updating dependencies..."
	poetry update

.PHONY: deps-show
deps-show:
	@echo "Showing dependencies..."
	poetry show

# Help target
.PHONY: help
help:
	@echo "GPU Health Monitor Makefile (Python) - Matching .gitlab-ci.yml exactly"
	@echo ""
	@echo "Configuration (environment variables):"
	@echo "  NVCR_CONTAINER_REPO=$(NVCR_CONTAINER_REPO)"
	@echo "  NGC_ORG=$(NGC_ORG)"
	@echo "  SAFE_REF_NAME=$(SAFE_REF_NAME)"
	@echo "  PLATFORMS=$(PLATFORMS)"
	@echo ""
	@echo "Main targets:"
	@echo "  all           - Run lint-test (default)"
	@echo "  lint-test     - Run full lint and test suite"
	@echo ""
	@echo "Individual targets:"
	@echo "  setup         - Set up Poetry environment"
	@echo "  lint          - Run Black formatter check"
	@echo "  format        - Run Black formatter"
	@echo "  test          - Run tests"
	@echo "  test-coverage - Run tests with coverage"
	@echo ""
	@echo "Docker targets:"
	@echo "  docker-build           - Build both DCGM 3.x and 4.x images locally"
	@echo "  docker-build-dcgm3     - Build GPU monitor with DCGM 3.x locally"
	@echo "  docker-build-dcgm4     - Build GPU monitor with DCGM 4.x locally"
	@echo "  docker-publish         - Build and push both images to registry (--push, matches GitLab CI)"
	@echo "  docker-publish-dcgm3   - Build and push DCGM 3.x image to registry"
	@echo "  docker-publish-dcgm4   - Build and push DCGM 4.x image to registry"
	@echo "  image                  - Legacy: Build both DCGM variants locally"
	@echo "  image-dcgm3            - Legacy: Build DCGM 3.x locally"
	@echo "  image-dcgm4            - Legacy: Build DCGM 4.x locally"
	@echo "  publish                - Legacy: Build locally then push both"
	@echo "  publish-dcgm3          - Legacy: Build locally then push DCGM 3.x"
	@echo "  publish-dcgm4          - Legacy: Build locally then push DCGM 4.x"
	@echo ""
	@echo "Development targets:"
	@echo "  shell         - Open Poetry shell"
	@echo "  deps          - Update dependencies"
	@echo "  deps-show     - Show dependencies"
	@echo "  setup-buildx  - Setup Docker buildx builder"
	@echo "  clean         - Clean build artifacts and reports"
	@echo "  help          - Show this help message"
	@echo ""
	@echo "Notes:"
	@echo "  - This is a Python module using Poetry for dependency management"
	@echo "  - Docker images support both DCGM 3.x and 4.x versions"
	@echo "  - Multi-platform builds: $(PLATFORMS)"
	@echo "  - Images tagged with dynamic ref name: $(SAFE_REF_NAME)"
	@echo "  - Build cache is used for faster builds"
	@echo "  - Matches .gitlab-ci.yml configuration exactly"