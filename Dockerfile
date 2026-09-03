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

# davfs2 tuning for a non-interactive, multi-pod mount. Shipped defaults would break or hurt:
#   ask_auth 1      → mount.davfs prompts for a username on stdin (no TTY → mount fails).
#   delay_upload 10 → a closed file waits 10s before being PUT; other pods see stale data.
#   dir_refresh 60  → directory listings cached 60s (on top of retyc-cli's own 30s cache).
#   use_locks 1     → WebDAV LOCKs are per retyc-webdav process, i.e. per node: they can't
#                     coordinate anything across nodes and only add round-trips.
COPY <<'EOF' /etc/davfs2/davfs2.conf
ask_auth      0
use_locks     0
delay_upload  0
dir_refresh   5
file_refresh  1
gui_optimize  0
EOF

COPY --from=retyc /retyc /usr/local/bin/retyc
COPY --from=builder /retyc-k8s-csi /usr/local/bin/retyc-k8s-csi

ENTRYPOINT ["/usr/local/bin/retyc-k8s-csi"]
