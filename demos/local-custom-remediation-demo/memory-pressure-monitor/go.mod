module github.com/nvidia/nvsentinel/demos/local-custom-remediation-demo/memory-pressure-monitor

go 1.26.0

toolchain go1.26.3

require (
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af
)

require (
	github.com/yandex/protoc-gen-crd v1.1.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
)

replace github.com/nvidia/nvsentinel/data-models => ../../../data-models
