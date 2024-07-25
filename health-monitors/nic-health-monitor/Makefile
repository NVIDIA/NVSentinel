all: binary

test:
	go test ./...

binary:
	go build -o nic-health-monitor cmd/nichealthmonitor.go

image:
	docker build -t nvcr.io/nv-ngc-devops/nvsentinel-nic-health-monitor:latest -f ./Dockerfile .
