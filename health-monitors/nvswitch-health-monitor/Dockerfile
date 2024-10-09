FROM golang:1.22 AS builder

ENV CGO_ENABLED=0

WORKDIR /go/src/nvswitch-health-monitor

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o nvswitch-health-monitor cmd/nvswitch_health_monitor.go


FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y pciutils

RUN apt-get install -y --no-install-recommends \
    e2fsprogs=1.46.2-2+deb11u1 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

ENTRYPOINT ["/app/nvswitch-health-monitor"]