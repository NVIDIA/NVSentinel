.PHONY: all binary image build-and-push clean test

all: binary

binary:
	go build -o health-event-client .

test:
	go test ./... -v

image:
	docker build -t nvcr.io/nv-ngc-devops/nvsentinel-health-event-client:main -f ./Dockerfile .

build-and-push:
	./build.sh

clean:
	rm -f health-event-client