all: binary

test:
	go test ./...

binary:
	go build -o nvswitch-health-monitor cmd/nvswitch_health_monitor.go

image:
	docker build -t nvcr.io/nv-ngc-devops/nvsentinel/nvswitch-monitor:latest .