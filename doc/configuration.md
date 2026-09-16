# Configuration

Everything below is exposed by the Helm chart (`charts/retyc-csi`); the value names are in its
[README](../charts/retyc-csi/README.md).

## Driver flags

| Flag | Default | Description |
|------|---------|-------------|
| `--mode` | | `controller` or `node` (required) |
| `--endpoint` | `unix:///csi/csi.sock` | CSI gRPC endpoint; only `unix://` is supported |
| `--retyc-bin` | `retyc` | path to the `retyc` CLI binary |
| `--http-endpoint` | `:9808` | address of the `/healthz`, `/readyz` and `/metrics` endpoints; empty disables them |
| `--node-id` | `$NODE_ID`, then the hostname | node ID reported by `NodeGetInfo` (node mode) |
| `--webdav-addr` | `127.0.0.1` | bind address of the supervised `retyc webdav serve` servers (node mode) |
| `--webdav-port` | `8888` | first port; each identity's server takes the next two free ones, WebDAV and probes (node mode) |
| `--state-dir` | `/var/lib/retyc-csi` | per-identity state of the `retyc` servers (node mode) |

`klog` flags (`-v`, `--logtostderr`, ...) are accepted as well.

## Environment

The driver passes its whole environment to every `retyc` subprocess, so the CLI's own variables apply, except the ones
it sets itself: `RETYC_KEYRING_ENABLED=false` everywhere, `RETYC_CONFIG_DIR` and the listen addresses per WebDAV
server. The two that matter are injected from the `retyc-csi-credentials` Secret with `envFrom`:

| Variable | Value |
|----------|-------|
| `RETYC_TOKEN` | offline token, minted once with `retyc auth login --offline` |
| `RETYC_KEY_PASSPHRASE` | passphrase of the account's AGE key |

`NODE_ID` is set by the DaemonSet from `spec.nodeName`.

Never mount a `token.json` (from `retyc auth login`) into the driver pods. When a `RETYC_TOKEN` is expired or revoked,
the CLI falls back to the token stored in its config directory, so a tenant whose token was revoked would run as the
account of that file. The driver itself never writes one: with `RETYC_TOKEN` set, the CLI keeps tokens in memory.

The driver's own environment is the **cluster-wide default identity**. It is optional: a driver started without
`RETYC_TOKEN` serves only StorageClasses that carry secret parameters (see below), and its controller readiness then
skips the `auth status` check.

## The Secret

The chart creates it from `credentials.token` and `credentials.keyPassphrase`, or uses the one named by
`credentials.existingSecret` (keys `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE`). A chart-managed Secret that changes
rolls both components automatically (checksum annotation). With an existing Secret, the pods read it at start, so
after changing it restart both components:

```sh
kubectl -n kube-system rollout restart deploy/retyc-csi-controller ds/retyc-csi-node
```

Restarting the node DaemonSet breaks the mounts on every node (see [architecture.md](architecture.md#failure-modes));
schedule it accordingly. A rollout of the controller with invalid credentials never replaces the running pod, because
the new one does not become Ready.

## StorageClasses

The chart ships three classes (`storageClasses` value).

| Name | Identity | `reclaimPolicy` | `volumeBindingMode` |
|------|----------|-----------------|---------------------|
| `retyc-rwx` | cluster-wide | `Delete` | `Immediate` |
| `retyc-rwx-retain` | cluster-wide | `Retain` | `Immediate` |
| `retyc-rwx-tenant` | the claim's namespace | `Retain` | `Immediate` |

Requested sizes are accepted and recorded on the PV but not enforced. All access modes are accepted
(`ReadWriteOnce`, `ReadOnlyMany`, `ReadWriteMany`); a read-only publish is bind-mounted read-only.

## Per-tenant identities

`retyc-rwx-tenant` uses the standard CSI secret parameters, templated on the claim's namespace:

```yaml
parameters:
  csi.storage.k8s.io/provisioner-secret-name: retyc-credentials
  csi.storage.k8s.io/provisioner-secret-namespace: ${pvc.namespace}
  csi.storage.k8s.io/node-stage-secret-name: retyc-credentials
  csi.storage.k8s.io/node-stage-secret-namespace: ${pvc.namespace}
```

A namespace using the class must hold a Secret named `retyc-credentials` with the same two keys as the driver's
Secret. The provisioner reads it for `CreateVolume` and `DeleteVolume` (it records the reference on the PV), kubelet
reads it for `NodeStageVolume`. On each node the driver runs one `retyc webdav serve` per identity in use, on two
consecutive loopback ports from `--webdav-port`, each with its own state directory under `--state-dir`; a tenant's
server stops when its last volume leaves the node.

What to know before rolling it out:

- the Secret's name is fixed by the StorageClass; change both if `retyc-credentials` does not suit;
- a claim in a namespace without the Secret stays `Pending`, with the provisioner event saying which Secret is missing;
- the class is `Retain` on purpose: a tenant that deletes its Secret before its claims would otherwise leave PVs stuck
  on a `DeleteVolume` that can no longer authenticate. Copy the class with `Delete` if that trade-off is acceptable;
- memory on the node plugin grows with the number of identities in use per node (about 40 MiB each, plus the transient
  scrypt peak). Adjust the DaemonSet limit accordingly.

Example, with a namespace, its Secret, a claim and a pod:
[`examples/tenant.yaml`](../examples/tenant.yaml).

## Adopting an existing dataroom

A dataroom that already exists - retained from a deleted claim, or created with `retyc dataroom create` - becomes a
volume through a static PV whose `volumeHandle` is the dataroom ID and whose `title` attribute is its name:

```sh
retyc --json dataroom ls          # id + title
```

Then fill in and apply [`examples/static-pv.yaml`](../examples/static-pv.yaml). Use `Retain` on a static
PV: the provisioner never deletes volumes it did not create.

## Health endpoints, probes and resources

Both modes serve `/healthz` (liveness) and `/readyz` (readiness) on `--http-endpoint` (default `:9808`), and the CSI
`Probe` RPC reports the same readiness.

- **Node**: ready when every supervised `retyc webdav serve` reports ready on its own loopback `/readyz`. When one
  does not, `/readyz` returns the process state and its last output line - e.g. `key passphrase check failed` - and
  the same line is logged.
- **Controller**: ready when `retyc auth status` reports an authenticated account (checked every two minutes).

Liveness is deliberately lenient on the node: restarting the plugin breaks every mount on that node, so a bad
credential shows up as a `1/2` NotReady pod, never as a restart loop. See [troubleshooting.md](troubleshooting.md).

Both driver containers expose port `9808`:

| Probe | Path | Period | Meaning |
|-------|------|--------|---------|
| liveness | `/healthz` | 30 s, 5 failures | the process serves HTTP at all |
| readiness | `/readyz` | 10 s, 2 failures | node: WebDAV servers ready on loopback; controller: `retyc auth status` authenticated (cached 2 min) |

Resources default to a 64 MiB request and a 512 MiB limit per driver container. Keep the node plugin's limit above
~350 MiB: unlocking the AGE key needs ~256 MiB transiently, and each tenant identity adds a server
(see [architecture.md](architecture.md#resource-profile)).

## Metrics

Both driver containers serve Prometheus metrics on `/metrics`, same port as the probes (`9808`, container port
`health`). With the Prometheus Operator, `metrics.podMonitor.enabled=true` creates a `PodMonitor` for both components
(add the labels your Prometheus selects on in `metrics.podMonitor.labels`); without it, scrape port `health` of the
pods labeled `app.kubernetes.io/name=retyc-csi`.

| Metric | Component | Meaning |
|--------|-----------|---------|
| `go_*`, `process_*` | both | runtime of the driver process |
| `retyc_csi_build_info{version, mode}` | both | constant 1 |
| `retyc_csi_webdav_server_up{identity, tenant}` | node | 1 when that server answered its scrape, 0 when it is down, starting or stuck |
| `retyc_cli_*{identity, tenant, ...}` | node | the metrics of every `retyc webdav serve` on the node: WebDAV requests and latency, bytes transferred, Retyc API calls and latency, cache efficiency, token refreshes and expiry, crypto time. See the CLI's [WebDAV metrics](https://github.com/retyc/retyc-cli/blob/master/doc/webdav.md#metrics) |

The node runs one server per identity, and two labels name it:

- `identity`: the same key as in `/readyz` and the logs, a hash of the credentials, safe to expose. Constant for the
  life of the server and unique on the node: use it to follow a server over time (`rate()`, `sum by (identity)`).
- `tenant`: who uses the server, for humans. It can change while the server runs.

| `tenant` | Server |
|----------|--------|
| `_default` | the driver's cluster-wide identity, whatever the namespaces of its volumes |
| `team-a` | a tenant identity, by the namespace of the claims staged through it on this node |
| `team-a,team-b` | the same tenant Secret values copied into several namespaces: one identity, one server |
| empty | a tenant whose volumes do not record their namespace (see below) |

Two different tenant Secrets in one namespace give two servers with the same `tenant` and different `identity`.
`identity` and `tenant` are reserved: the driver overwrites them on every series, including a
`RETYC_WEBDAV_METRICS_LABELS` set in `node.extraEnv`.

The controller records the claim's namespace in the PV's `volumeAttributes` (`pvcNamespace`, from csi-provisioner's
`--extra-create-metadata`). PVs provisioned before that have none, and PV attributes cannot be changed: their tenant
stays empty until they are recreated. A static PV using a tenant Secret can set `pvcNamespace` itself
([`examples/static-pv.yaml`](../examples/static-pv.yaml)); a value that is not a namespace name is ignored with a
warning.

A tenant's label follows the volumes staged on the node, which the node plugin only learns at `NodeStageVolume`. After
a node plugin restart - which breaks the tenants' mounts anyway, see [troubleshooting.md](troubleshooting.md) - their
servers do not reappear in the metrics until a volume of that identity is staged again.

The node plugin scrapes its servers on loopback at each scrape of its own `/metrics` (concurrently, 3 s each, two
scrapes of `/metrics` at most at once); a server that does not answer only loses its `retyc_cli_*` series for that
scrape and reports `up` 0. `/metrics` listens on the pod IP like the probes, without authentication: it carries
namespace names, identity keys, normalized API routes and versions, no dataroom content. On a cluster that restricts
pod traffic, allow ingress to port `9808` from Prometheus only, e.g. with a NetworkPolicy in `extraObjects`.

Useful starting points:

```promql
retyc_csi_webdav_server_up == 0                                                     # a server down on a node
sum by (identity, tenant) (rate(retyc_cli_webdav_requests_total{status=~"5.."}[5m])) # WebDAV errors per server
histogram_quantile(0.95, sum by (le, route) (rate(retyc_cli_api_request_duration_seconds_bucket[5m])))
retyc_cli_token_expiry_seconds < 300                                                # access token about to expire
```

## Image

The `ghcr.io/retyc/retyc-cli` release embedded in the image is pinned by `ARG RETYC_CLI_VERSION` in the `Dockerfile`, the
single place to bump it (Dependabot's `docker` ecosystem proposes the updates). For a one-off build against another
release:

```sh
make image RETYC_CLI_VERSION=v1.3.0
```

The image is based on `debian:trixie-slim` with `davfs2`, `libnss-unknown`, CA certificates and the `/etc/mtab`
symlink that `mount.davfs` requires under containerd.

## Installing without Helm

`helm template` renders the same resources as plain YAML, for GitOps pipelines or a `kubectl apply`:

```sh
helm template retyc-csi retyc/retyc-csi -n kube-system --set credentials.existingSecret=retyc-csi-credentials
```

The chart is also available as an OCI artifact: `helm install retyc-csi oci://ghcr.io/retyc/charts/retyc-csi`.
