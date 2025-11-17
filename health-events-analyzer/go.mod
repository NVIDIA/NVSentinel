module github.com/nvidia/nvsentinel/health-events-analyzer

go 1.25

toolchain go1.25.3

require (
	github.com/hashicorp/go-multierror v1.1.1
	github.com/nvidia/nvsentinel/commons v0.0.0
	github.com/nvidia/nvsentinel/data-models v0.0.0
	github.com/nvidia/nvsentinel/store-client v0.0.0
	github.com/prometheus/client_golang v1.23.2
	github.com/stretchr/testify v1.11.1
	golang.org/x/sync v0.18.0
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
	k8s.io/apimachinery v0.34.1
)

require (
	github.com/hashicorp/errwrap v1.1.0 // indirect
	go.yaml.in/yaml/v2 v2.4.3 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/utils v0.0.0-20251002143259-bc988d571ff4 // indirect
)

require (
	github.com/BurntSushi/toml v1.5.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/klauspost/compress v1.18.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/montanaflynn/stats v0.7.1 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/client_model v0.6.2 // indirect
	github.com/prometheus/common v0.67.2 // indirect
	github.com/prometheus/procfs v0.17.0 // indirect
	github.com/stretchr/objx v0.5.2 // indirect
	github.com/xdg-go/pbkdf2 v1.0.0 // indirect
	github.com/xdg-go/scram v1.1.2 // indirect
	github.com/xdg-go/stringprep v1.0.4 // indirect
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78 // indirect
	go.mongodb.org/mongo-driver v1.17.4 // indirect
	golang.org/x/crypto v0.43.0 // indirect
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251022142026-3a174f9686a8 // indirect
	k8s.io/klog/v2 v2.130.1 // indirect
)

// Local replacements for internal modules
replace github.com/nvidia/nvsentinel/store-client => ../store-client

replace github.com/nvidia/nvsentinel/data-models => ../data-models

replace github.com/nvidia/nvsentinel/commons => ../commons
