module github.com/nvidia/nvsentinel/event-exporter

go 1.25

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/google/uuid v1.6.0
)

replace (
	github.com/nvidia/nvsentinel/commons => ../commons
	github.com/nvidia/nvsentinel/data-models => ../data-models
	github.com/nvidia/nvsentinel/store-client => ../store-client
)
