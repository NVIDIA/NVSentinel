module github.com/nvidia/nvsentinel/janitor

go 1.24.0

toolchain go1.24.8

require k8s.io/klog/v2 v2.130.1

require github.com/go-logr/logr v1.4.3 // indirect

// Local replacements for internal modules
replace github.com/nvidia/nvsentinel/statemanager => ../statemanager

replace github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor => ../health-monitors/csp-health-monitor

replace github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor => ../health-monitors/syslog-health-monitor

replace github.com/nvidia/nvsentinel/platform-connectors => ../platform-connectors

replace github.com/nvidia/nvsentinel/store-client-sdk => ../store-client-sdk

replace github.com/nvidia/nvsentinel/health-event-client => ../health-event-client

replace github.com/nvidia/nvsentinel/health-events-analyzer => ../health-events-analyzer

replace github.com/nvidia/nvsentinel/fault-quarantine-module => ../fault-quarantine-module

replace github.com/nvidia/nvsentinel/labeler-module => ../labeler-module

replace github.com/nvidia/nvsentinel/node-drainer-module => ../node-drainer-module

replace github.com/nvidia/nvsentinel/fault-remediation-module => ../fault-remediation-module
