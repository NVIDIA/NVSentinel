# Tests Makefile
# End-to-end test targets

# Go binary
GO := go

# Default target
.PHONY: all
all: test

# Run end-to-end tests in CI
.PHONY: test-ci
test-ci:
	nohup kubectl port-forward -n monitoring svc/prometheus-operator-kube-p-prometheus 9090:9090 > /dev/null 2>&1 &
	nohup kubectl port-forward -n nvsentinel svc/simple-health-client 8080:8080 > /dev/null 2>&1 &
	kubectl get po -Ao wide
	kubectl get no -o wide
	@echo "Pre-test: Ensuring MongoDB initialization is complete..."
	kubectl wait --for=condition=complete job/create-mongodb-database -n nvsentinel --timeout=5m || true
	@echo "Pre-test: Cleaning up any failed pods..."
	kubectl delete pods -n nvsentinel --field-selector=status.phase=Failed --ignore-not-found=true || true
	@echo "Pre-test: Waiting for fault-quarantine deployment to be available..."
	kubectl wait --for=condition=available deployment/nvsentinel-fault-quarantine -n nvsentinel --timeout=5m || true
	@sleep 10
	@echo "Restarting fault-quarantine deployment..."
	kubectl rollout restart deploy -l app.kubernetes.io/name=fault-quarantine -n nvsentinel
	@echo "Waiting for deployment rollout to complete..."
	kubectl rollout status deployment/nvsentinel-fault-quarantine -n nvsentinel --timeout=10m
	$(MAKE) test

# Run end-to-end tests
.PHONY: test
test:
	gotestsum --format standard-verbose -- -timeout 40m -count 1 -failfast ./...

# Help target
.PHONY: help
help:
	@echo "Tests Makefile - Simple end-to-end test execution"
	@echo ""
	@echo "Targets:"
	@echo "  all       - Run test (default)"
	@echo "  test-ci   - Run end-to-end tests in CI environment"
	@echo "  test      - Run end-to-end tests"
	@echo "  help      - Show this help message"
