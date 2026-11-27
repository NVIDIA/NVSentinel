module github.com/nvidia/nvsentinel/health-monitors/nvhv-health-monitor

go 1.27.3

require (
	github.com/nvidia/nvsentinel/commons v0.0.0
	github.com/stretchr/testify v1.12.1
	google.golang.org/grpc v1.85.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.yaml.in/yaml/v3 v3.0.6 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261104224025-d1e00e9f33ab // indirect
)

replace github.com/nvidia/nvsentinel/commons => ../../commons

replace github.com/nvidia/nvsentinel/data-models => ../../data-models
