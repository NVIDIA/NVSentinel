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
	$(MAKE) test

# Run end-to-end tests
.PHONY: test
test:
	$(GO) test -timeout 20m -count 1 -v ./...

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
