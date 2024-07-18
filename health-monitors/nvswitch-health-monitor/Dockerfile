FROM golang:1.22 as builder

ENV CGO_ENABLED=0

WORKDIR /go/src/nvswitch-health-monitor

COPY . .

RUN make

FROM ubuntu:24.10

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

RUN apt update
RUN apt install -y pciutils

ENTRYPOINT ["/app/nvswitch-health-monitor"]
