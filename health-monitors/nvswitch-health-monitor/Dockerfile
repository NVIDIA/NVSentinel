ARG TAG=v3.0.5
ARG IMG=nvcr.io/nvidian/distroless/go
FROM golang:1.22 as builder

ENV CGO_ENABLED=0

WORKDIR /go/src/nvswitch-health-monitor

COPY . .

RUN make

FROM debian:bullseye-slim as build1
# Install pciutils
RUN apt-get update && apt-get install -y pciutils
FROM $IMG:$TAG

COPY --from=build1 /usr/bin/lspci /usr/bin/lspci
COPY --from=build1 /usr/bin/setpci /usr/bin/setpci
COPY --from=build1 /usr/lib/x86_64-linux-gnu/libpci.so.3 /usr/lib/x86_64-linux-gnu/libpci.so.3
COPY --from=build1 /usr/lib/x86_64-linux-gnu/libkmod.so.2 /usr/lib/x86_64-linux-gnu/libkmod.so.2
COPY --from=build1 /lib/x86_64-linux-gnu/libc.so.6 /lib/x86_64-linux-gnu/libc.so.6
COPY --from=build1 /lib/x86_64-linux-gnu/libz.so.1 /lib/x86_64-linux-gnu/libz.so.1
COPY --from=build1 /lib/x86_64-linux-gnu/libresolv.so.2 /lib/x86_64-linux-gnu/libresolv.so.2
COPY --from=build1 /usr/lib/x86_64-linux-gnu/libudev.so.1 /usr/lib/x86_64-linux-gnu/libudev.so.1
COPY --from=build1 /lib/x86_64-linux-gnu/liblzma.so.5 /lib/x86_64-linux-gnu/liblzma.so.5
COPY --from=build1 /usr/lib/x86_64-linux-gnu/libcrypto.so.1.1 /usr/lib/x86_64-linux-gnu/libcrypto.so.1.1
COPY --from=build1 /lib64/ld-linux-x86-64.so.2 /lib64/ld-linux-x86-64.so.2
COPY --from=build1 /lib/x86_64-linux-gnu/libpthread.so.0 /lib/x86_64-linux-gnu/libpthread.so.0
COPY --from=build1 /lib/x86_64-linux-gnu/libdl.so.2 /lib/x86_64-linux-gnu/libdl.so.2

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

ENTRYPOINT ["/app/nvswitch-health-monitor"]