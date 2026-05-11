module github.com/nvidia/nvsentinel/demos/local-custom-remediation-demo/memory-pressure-monitor

go 1.26.0

toolchain go1.26.2

require (
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.81.0
	google.golang.org/protobuf v1.36.11
)

require (
	github.com/yandex/protoc-gen-crd v1.1.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260226221140-a57be14db171 // indirect
)

replace github.com/nvidia/nvsentinel/data-models => ../../../data-models
