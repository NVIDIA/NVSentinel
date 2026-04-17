module github.com/nvidia/nvsentinel/demos/local-custom-remediation-demo/memory-pressure-monitor

go 1.25.0

require (
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/yandex/protoc-gen-crd v1.1.0 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	golang.org/x/net v0.49.0 // indirect
	golang.org/x/sys v0.40.0 // indirect
	golang.org/x/text v0.33.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260209200024-4cfbd4190f57 // indirect
)

replace github.com/nvidia/nvsentinel/data-models => ../../../data-models
