module github.com/nvidia/nvsentinel/preflight-checks/nccl-loopback

go 1.25.0

require (
	github.com/nvidia/nvsentinel/commons v0.0.0
	github.com/nvidia/nvsentinel/data-models v0.0.0
	google.golang.org/grpc v1.77.0
	google.golang.org/protobuf v1.36.11
	k8s.io/apimachinery v0.35.1
)

require (
	github.com/go-logr/logr v1.4.3 // indirect
	golang.org/x/net v0.47.0 // indirect
	golang.org/x/sys v0.38.0 // indirect
	golang.org/x/text v0.31.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251124214823-79d6a2a48846 // indirect
	k8s.io/klog/v2 v2.130.1 // indirect
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4 // indirect
)

replace github.com/nvidia/nvsentinel/commons => ../../commons

replace github.com/nvidia/nvsentinel/data-models => ../../data-models
