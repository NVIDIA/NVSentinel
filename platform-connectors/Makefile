all: binary

binary:
	go build -o platform-connectors  main.go

image:
	docker build -t nvcr.io/nv-ngc-devops/nvsentinel/platform-connectors:latest -f ./Dockerfile .

