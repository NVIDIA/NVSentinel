module github.com/nvidia/nvsentinel/tests/scale-tests/event-generator

go 1.27.0

require (
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.83.2
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/yandex/protoc-gen-crd v1.1.0 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	go.opentelemetry.io/otel/sdk v1.46.0 // indirect
	go.opentelemetry.io/otel/trace v1.46.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260825221802-da73d73af1c5 // indirect
)

// Use local data-models from same repo
// Pinned to commit ee6c06bb87e28f34dfffe0a999eaf7fb4366eb5b (November 21, 2025)
// If data-models API changes, update this code and re-pin to new commit
replace github.com/nvidia/nvsentinel/data-models => ../../../data-models
