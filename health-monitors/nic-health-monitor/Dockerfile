ARG TAG=v3.0.5
ARG IMG=nvcr.io/nvidian/distroless/go

FROM golang:1.22 as builder

ENV CGO_ENABLED=0

WORKDIR /go/src/nic-health-monitor

COPY . .

RUN make

FROM $IMG:$TAG

COPY --from=builder /go/src/nic-health-monitor/nic-health-monitor /app/nic-health-monitor

ENTRYPOINT ["/app/nic-health-monitor"]
