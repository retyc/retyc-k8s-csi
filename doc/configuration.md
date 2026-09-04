# Configuration

## Driver flags

| Flag | Default | Description |
|------|---------|-------------|
| `--mode` | | `controller` or `node` (required) |
| `--endpoint` | `unix:///csi/csi.sock` | CSI gRPC endpoint; only `unix://` is supported |
| `--retyc-bin` | `retyc` | path to the `retyc` CLI binary |
| `--http-endpoint` | `:9808` | address of the `/healthz` and `/readyz` endpoints; empty disables them |
| `--node-id` | `$NODE_ID`, then the hostname | node ID reported by `NodeGetInfo` (node mode) |
| `--webdav-addr` | `127.0.0.1` | bind address of the supervised `retyc webdav serve` (node mode) |
| `--webdav-port` | `8888` | its port (node mode) |

`klog` flags (`-v`, `--logtostderr`, ...) are accepted as well.

## Environment

The driver passes its whole environment to every `retyc` subprocess, so the CLI's own variables apply. The two that
matter are injected from the `retyc-csi-credentials` Secret with `envFrom`:

| Variable | Value |
|----------|-------|
| `RETYC_TOKEN` | offline token, minted once with `retyc auth login --offline` |
| `RETYC_KEY_PASSPHRASE` | passphrase of the account's AGE key |

`NODE_ID` is set by the DaemonSet from `spec.nodeName`.

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

`deploy/storageclass.yaml` ships two classes. Neither takes parameters.

| Name | `reclaimPolicy` | `volumeBindingMode` |
|------|-----------------|---------------------|
| `retyc-rwx` | `Delete` | `Immediate` |
| `retyc-rwx-retain` | `Retain` | `Immediate` |

Requested sizes are accepted and recorded on the PV but not enforced. All access modes are accepted
(`ReadWriteOnce`, `ReadOnlyMany`, `ReadWriteMany`); a read-only publish is bind-mounted read-only.

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
~350 MiB: unlocking the AGE key needs ~256 MiB transiently (see [architecture.md](architecture.md#resource-profile)).

## Image

`RETYC_VERSION` pins the `retyc/retyc-cli` image tag embedded at build time:

```sh
make image RETYC_VERSION=v1.2.0
```

The image is based on `debian:trixie-slim` with `davfs2`, `libnss-unknown`, CA certificates and the `/etc/mtab`
symlink that `mount.davfs` requires under containerd.
