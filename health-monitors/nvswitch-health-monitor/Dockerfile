ARG TAG=v3.0.5
ARG IMG=nvcr.io/nvidian/distroless/go

FROM golang:1.22 as builder

ENV CGO_ENABLED=0

RUN mkdir -p /etc/config
WORKDIR /go/src/nvswitch-health-monitor

COPY go.mod go.sum ./
# RUN go mod download

COPY . .

RUN go build -o nvswitch-health-monitor cmd/nvswitchhealthmonitor.go

FROM $IMG:$TAG

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

ENTRYPOINT ["/app/nvswitch-health-monitor"]
