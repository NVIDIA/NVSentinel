module github.com/nvidia/nvsentinel/metadata-collector

go 1.25

toolchain go1.25.3

require (
	github.com/NVIDIA/go-nvml v0.13.0-1
	github.com/nvidia/nvsentinel/commons v0.0.0
	github.com/nvidia/nvsentinel/data-models v0.0.0
	github.com/stretchr/testify v1.11.1
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251014184007-4626949a642f // indirect
	google.golang.org/grpc v1.76.0 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

replace (
	github.com/nvidia/nvsentinel/commons => ../commons
	github.com/nvidia/nvsentinel/data-models => ../data-models
)
