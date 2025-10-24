module simple-health-client

go 1.24.0

toolchain go1.24.8

require (
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
)

require (
	go.opentelemetry.io/otel/metric v1.38.0 // indirect
	go.opentelemetry.io/otel/trace v1.38.0 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251014184007-4626949a642f // indirect
)

// Local replacements for internal modules
replace github.com/nvidia/nvsentinel/data-models => ../../data-models

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
