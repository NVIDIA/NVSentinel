module github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor

go 1.24.0

toolchain go1.24.8

require (
	github.com/coreos/go-systemd/v22 v22.5.0
	github.com/hashicorp/go-retryablehttp v0.7.8
	github.com/prometheus/client_golang v1.23.2
	github.com/stretchr/testify v1.11.1
	github.com/thedatashed/xlsxreader v1.2.8
	google.golang.org/grpc v1.75.0
	google.golang.org/protobuf v1.36.8
	gopkg.in/yaml.v3 v3.0.1
	k8s.io/apimachinery v0.34.0
	k8s.io/klog v1.0.0
	k8s.io/klog/v2 v2.130.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.66.1 // indirect
	github.com/prometheus/procfs v0.16.1 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
	golang.org/x/text v0.28.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20250707201910-8d1bb00bc6a7 // indirect
	k8s.io/utils v0.0.0-20250604170112-4c0f3b243397 // indirect
)

// Local replacements for internal modules
replace github.com/nvidia/nvsentinel/statemanager => ../../statemanager

replace github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor => ../../health-monitors/csp-health-monitor

replace github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor => ../../health-monitors/syslog-health-monitor

replace github.com/nvidia/nvsentinel/platform-connectors => ../../platform-connectors

replace github.com/nvidia/nvsentinel/store-client-sdk => ../../store-client-sdk

replace github.com/nvidia/nvsentinel/health-event-client => ../../health-event-client

replace github.com/nvidia/nvsentinel/health-events-analyzer => ../../health-events-analyzer

replace github.com/nvidia/nvsentinel/fault-quarantine-module => ../../fault-quarantine-module

replace github.com/nvidia/nvsentinel/labeler-module => ../../labeler-module

replace github.com/nvidia/nvsentinel/node-drainer-module => ../../node-drainer-module

replace github.com/nvidia/nvsentinel/fault-remediation-module => ../../fault-remediation-module
