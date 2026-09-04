# Configuration

Everything below is exposed by the Helm chart (`charts/retyc-csi`, values in its
[README](../charts/retyc-csi/README.md)) and set by hand in the raw manifests under `deploy/`.

## Driver flags

| Flag | Default | Description |
|------|---------|-------------|
| `--mode` | | `controller` or `node` (required) |
| `--endpoint` | `unix:///csi/csi.sock` | CSI gRPC endpoint; only `unix://` is supported |
| `--retyc-bin` | `retyc` | path to the `retyc` CLI binary |
| `--http-endpoint` | `:9808` | address of the `/healthz` and `/readyz` endpoints; empty disables them |
| `--node-id` | `$NODE_ID`, then the hostname | node ID reported by `NodeGetInfo` (node mode) |
| `--webdav-addr` | `127.0.0.1` | bind address of the supervised `retyc webdav serve` servers (node mode) |
| `--webdav-port` | `8888` | first port; each identity's server takes the next free one (node mode) |
| `--state-dir` | `/var/lib/retyc-csi` | per-identity state of the `retyc` servers (node mode) |

`klog` flags (`-v`, `--logtostderr`, ...) are accepted as well.

## Environment

The driver passes its whole environment to every `retyc` subprocess, so the CLI's own variables apply. The two that
matter are injected from the `retyc-csi-credentials` Secret with `envFrom`:

| Variable | Value |
|----------|-------|
| `RETYC_TOKEN` | offline token, minted once with `retyc auth login --offline` |
| `RETYC_KEY_PASSPHRASE` | passphrase of the account's AGE key |

`NODE_ID` is set by the DaemonSet from `spec.nodeName`.

The driver's own environment is the **cluster-wide default identity**. It is optional: a driver started without
`RETYC_TOKEN` serves only StorageClasses that carry secret parameters (see below), and its controller readiness then
skips the `auth status` check.

## The Secret

```sh
cp deploy/secret.yaml.example deploy/secret.yaml
$EDITOR deploy/secret.yaml
kubectl apply -f deploy/secret.yaml
```

The Secret is read at pod start. After changing it, restart both components:

```sh
kubectl -n kube-system rollout restart deploy/retyc-csi-controller ds/retyc-csi-node
```

Restarting the node DaemonSet breaks the mounts on every node (see [architecture.md](architecture.md#failure-modes));
schedule it accordingly. A rollout of the controller with invalid credentials never replaces the running pod, because
the new one does not become Ready.

## StorageClasses

`deploy/storageclass.yaml` ships three classes.

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
reads it for `NodeStageVolume`. On each node the driver runs one `retyc webdav serve` per identity in use, on
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
[`deploy/examples/tenant.yaml.example`](../deploy/examples/tenant.yaml.example).

## Adopting an existing dataroom

A dataroom that already exists - retained from a deleted claim, or created with `retyc dataroom create` - becomes a
volume through a static PV whose `volumeHandle` is the dataroom ID and whose `title` attribute is its name:

```sh
retyc --json dataroom ls          # id + title
```

Then fill in and apply [`deploy/examples/static-pv.yaml`](../deploy/examples/static-pv.yaml). Use `Retain` on a static
PV: the provisioner never deletes volumes it did not create.

## Probes and resources

Both driver containers expose port `9808`:

| Probe | Path | Period | Meaning |
|-------|------|--------|---------|
| liveness | `/healthz` | 30 s, 5 failures | the process serves HTTP at all |
| readiness | `/readyz` | 10 s, 2 failures | node: WebDAV server answering on loopback; controller: `retyc auth status` authenticated (cached 2 min) |

Resources default to a 64 MiB request and a 512 MiB limit per driver container. Keep the node plugin's limit above
~350 MiB: unlocking the AGE key needs ~256 MiB transiently, and each tenant identity adds a server
(see [architecture.md](architecture.md#resource-profile)).

## Image

`RETYC_VERSION` pins the `retyc/retyc-cli` image tag embedded at build time:

```sh
make image RETYC_VERSION=v1.2.0
```

The image is based on `debian:trixie-slim` with `davfs2`, `libnss-unknown`, CA certificates and the `/etc/mtab`
symlink that `mount.davfs` requires under containerd.
