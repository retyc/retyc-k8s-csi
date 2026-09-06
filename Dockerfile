# The retyc-cli release embedded in the image — the single source of truth for that version.
# Bump it here (Dependabot proposes it); `make image RETYC_CLI_VERSION=...` overrides for a
# one-off build.
ARG RETYC_CLI_VERSION=v1.2.0-rc2
# Build provenance, set by `make image` (git describe / rev-parse / date -u); also reported by the
# driver's GetPluginInfo through ldflags.
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=1970-01-01T00:00:00Z

FROM golang:1.26-trixie AS builder
ARG VERSION

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X github.com/retyc/retyc-k8s-csi/internal/driver.DriverVersion=${VERSION}" \
    -o /retyc-k8s-csi .

# Reuse the officially published retyc-cli image instead of vendoring/rebuilding the binary —
# dataroom creation and WebDAV serving are security-sensitive (AGE crypto) and should always run
# the same vetted build the CLI team ships. Pulled from GitHub's registry (same digest as the
# Docker Hub mirror, no anonymous pull limits, provenance attached to the source repository).
FROM ghcr.io/retyc/retyc-cli:${RETYC_CLI_VERSION} AS retyc

FROM debian:trixie-slim
ARG RETYC_CLI_VERSION
ARG VERSION
ARG REVISION
ARG CREATED

LABEL org.opencontainers.image.title="Retyc CSI Driver" \
      org.opencontainers.image.description="Kubernetes CSI driver for Retyc datarooms: ReadWriteMany volumes with end-to-end post-quantum encryption" \
      org.opencontainers.image.vendor="Retyc / TripleStack SAS" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.url="https://retyc.com" \
      org.opencontainers.image.source="https://github.com/retyc/retyc-k8s-csi" \
      org.opencontainers.image.documentation="https://github.com/retyc/retyc-k8s-csi/blob/master/README.md" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.base.name="docker.io/library/debian:trixie-slim" \
      com.retyc.cli.version="${RETYC_CLI_VERSION}"

# libnss-unknown: davfs2 enforces permissions itself (the kernel is told allow_other without
# default_permissions) and, for any uid that is neither 0 nor the mount owner, its check starts
# with getpwuid(uid) and denies everything when that fails (cache.c, has_permission). The daemon
# runs in *this* container, so a pod's runAsUser would never resolve here without an NSS module
# that synthesises an entry for unknown uids. The package rewrites nsswitch.conf on install —
# and once it is active, davfs2's postinst sees `getent passwd davfs2` succeed for the
# not-yet-created account and skips creating it ("group davfs2 does not exist" at mount time),
# hence the two separate installs, davfs2 first.
RUN apt-get update && \
    apt-get install -y --no-install-recommends davfs2 ca-certificates && \
    apt-get install -y --no-install-recommends libnss-unknown && \
    rm -rf /var/lib/apt/lists/* && \
    # mount.davfs refuses to run without /etc/mtab ("can't access file /etc/mtab"). `docker run`
    # creates this symlink at container start, containerd (k3s, CRI) does not — so ship it.
    ln -sf /proc/mounts /etc/mtab

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
