# RETYC_VERSION pins the retyc-cli image tag to embed (e.g. "v0.3.0"); defaults to latest.
ARG RETYC_VERSION=latest

FROM golang:1.26-trixie AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /retyc-k8s-csi .

# Reuse the officially published retyc-cli image instead of vendoring/rebuilding the binary —
# dataroom creation and WebDAV serving are security-sensitive (AGE crypto) and should always run
# the same vetted build the CLI team ships.
FROM retyc/retyc-cli:${RETYC_VERSION} AS retyc

FROM debian:trixie-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends davfs2 ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=retyc /retyc /usr/local/bin/retyc
COPY --from=builder /retyc-k8s-csi /usr/local/bin/retyc-k8s-csi

ENTRYPOINT ["/usr/local/bin/retyc-k8s-csi"]
