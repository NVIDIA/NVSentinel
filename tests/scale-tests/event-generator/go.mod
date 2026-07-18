module github.com/nvidia/nvsentinel/tests/scale-tests/event-generator

go 1.26.0

toolchain go1.26.3

require (
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.82.0
	google.golang.org/protobuf v1.36.12-0.20260120151049-f2248ac996af
)

require (
	github.com/yandex/protoc-gen-crd v1.1.0 // indirect
	go.opentelemetry.io/otel/metric v1.44.0 // indirect
	go.opentelemetry.io/otel/sdk v1.44.0 // indirect
	go.opentelemetry.io/otel/trace v1.44.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/sys v0.46.0 // indirect
	golang.org/x/text v0.38.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260630182238-925bb5da69e7 // indirect
)

// Use local data-models from same repo
// Pinned to commit ee6c06bb87e28f34dfffe0a999eaf7fb4366eb5b (November 21, 2025)
// If data-models API changes, update this code and re-pin to new commit
replace github.com/nvidia/nvsentinel/data-models => ../../../data-models
