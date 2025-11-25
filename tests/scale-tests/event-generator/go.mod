module github.com/nvidia/nvsentinel/tests/scale-tests/event-generator

go 1.25

require (
	github.com/nvidia/nvsentinel/data-models v0.3.0
	google.golang.org/grpc v1.76.0
	google.golang.org/protobuf v1.36.10
)

require (
	golang.org/x/net v0.46.0 // indirect
	golang.org/x/sys v0.37.0 // indirect
	golang.org/x/text v0.30.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251022142026-3a174f9686a8 // indirect
)

// Use local data-models from same repo
// Pinned to commit ee6c06bb87e28f34dfffe0a999eaf7fb4366eb5b (November 21, 2025)
// If data-models API changes, update this code and re-pin to new commit
replace github.com/nvidia/nvsentinel/data-models => ../../../data-models
