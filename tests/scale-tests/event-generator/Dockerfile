FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the pre-built binary from local build
# Build with: go build -o event-generator .
COPY event-generator ./

ENTRYPOINT ["./event-generator"]
