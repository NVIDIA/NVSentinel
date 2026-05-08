module github.com/nvidia/nvsentinel/preflight-checks/nccl-loopback

go 1.26.0

toolchain go1.26.2

require (
	github.com/nvidia/nvsentinel/commons v0.0.0
	github.com/nvidia/nvsentinel/data-models v0.0.0
	go.opentelemetry.io/otel v1.43.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.43.0
	go.opentelemetry.io/otel/metric v1.43.0
	go.opentelemetry.io/otel/sdk v1.43.0
	go.opentelemetry.io/otel/sdk/metric v1.43.0
	google.golang.org/grpc v1.80.0
	google.golang.org/protobuf v1.36.11
	k8s.io/apimachinery v0.35.4
)

require (
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/yandex/protoc-gen-crd v1.1.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	golang.org/x/net v0.53.0 // indirect
	golang.org/x/sys v0.43.0 // indirect
	golang.org/x/text v0.36.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260401024825-9d38bb4040a9 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260401024825-9d38bb4040a9 // indirect
	k8s.io/klog/v2 v2.140.0 // indirect
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4 // indirect
)

replace github.com/nvidia/nvsentinel/commons => ../../commons

replace github.com/nvidia/nvsentinel/data-models => ../../data-models
