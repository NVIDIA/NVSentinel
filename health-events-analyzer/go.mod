module github.com/nvidia/nvsentinel/health-events-analyzer

go 1.24.0

toolchain go1.24.8

require (
	github.com/nvidia/nvsentinel/platform-connectors v0.0.0-20250326070254-068ef4e9b24f
	github.com/nvidia/nvsentinel/store-client-sdk v0.0.0-20250901050746-391b5ab7018d
	github.com/prometheus/client_golang v1.23.2
	github.com/stretchr/testify v1.11.1
	go.mongodb.org/mongo-driver v1.17.4
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
	k8s.io/apimachinery v0.34.1
)

require (
	github.com/kr/text v0.2.0 // indirect
	go.opentelemetry.io/otel/metric v1.38.0 // indirect
	go.opentelemetry.io/otel/trace v1.38.0 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4 // indirect
)

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/montanaflynn/stats v0.7.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.1 // indirect
	github.com/prometheus/procfs v0.17.0 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	golang.org/x/crypto v0.43.0 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251014184007-4626949a642f // indirect
	k8s.io/client-go v0.34.1 // indirect
	k8s.io/klog/v2 v2.130.1
)

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
