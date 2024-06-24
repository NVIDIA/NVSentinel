all: binary

binary:
	go build -o nic-health-monitor  main.go

image:
	docker build -t nvcr.io/nv-ngc-devops/nvsentinel-nic-health-monitor:latest -f ./Dockerfile .

