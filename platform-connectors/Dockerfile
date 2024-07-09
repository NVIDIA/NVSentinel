ARG TAG=v3.0.5
ARG IMG=nvcr.io/nvidian/distroless/go

FROM golang:1.22 AS builder

ENV CGO_ENABLED=0

RUN mkdir -p /etc/config
WORKDIR /go/src/platform-connectors

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o platform-connectors main.go

FROM $IMG:$TAG

COPY --from=builder /go/src/platform-connectors/platform-connectors /app/platform-connectors

ENTRYPOINT ["/app/platform-connectors"]
