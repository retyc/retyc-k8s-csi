# retyc-k8s-csi (POC)

A Kubernetes CSI driver that provisions RWX `PersistentVolume`s backed by [Retyc](https://retyc.com)
datarooms. One PV = one dataroom, mounted node-side through `retyc-cli`'s built-in WebDAV server
and `davfs2`.

See [`~/.claude/plans/buzzing-juggling-fern.md`](/home/triplestack/.claude/plans/buzzing-juggling-fern.md)
for the full design, the trade-offs accepted for this POC, and the staged verification plan.

## Status

This is a POC, not production-ready:

- **Eventually consistent, not POSIX-strict RWX.** The WebDAV lock system is in-memory/per-process
  and directory listings are cached ~30s — fine for sharing config/small artefacts between pods,
  not for high-churn concurrent writes to the same file.
- **No resize, no snapshots, no per-dataroom capacity enforcement.** Requested PVC sizes are
  accepted and echoed back but not actually limited by the backend.
- **Single shared Retyc identity for the whole cluster** — no per-tenant credentials.
- **WebDAV `--auth` is deliberately off**: the supervised `retyc webdav serve` and the `davfs2`
  mount both run inside the same node-plugin container/netns, so loopback-only really is the
  trust boundary here.
- **Mounts are world-writable inside the pod** (`dir_mode=0777,file_mode=0666`): `fsGroup`
  can't be applied to a WebDAV/FUSE mount, so this is how non-root pods get write access. Two
  caveats: those modes only apply to entries davfs2 discovers on the server; a file *created*
  through the mount keeps the mode the kernel derived from the creating pod's umask (typically
  0644 for root), until the plugin restarts and the metadata cache is rebuilt. And davfs2 does
  its own permission checks, which start with `getpwuid()` of the calling uid *inside the plugin
  container* — the image ships `libnss-unknown` so that arbitrary `runAsUser` values resolve.
- **A node-plugin pod restart kills every davfs2 mount on that node.** The FUSE daemons live in
  the plugin container; pods then see "Transport endpoint is not connected" until they are
  rescheduled/remounted (the driver does clean up the corrupted mounts on the next
  stage/unstage). Production FUSE-based CSI drivers work around this by running the daemon on
  the host — out of scope for this POC.
- **`CreateVolume` idempotency only sees the first page of `retyc dataroom ls`** (the CLI has no
  `--page` flag); a retried create past that page can produce a duplicate dataroom. Logged as a
  warning when it becomes possible.
- Validated end-to-end on a single-node k3s (Debian trixie VM, see [`doc/testing.md`](doc/testing.md)):
  PVC bound, davfs2 mount propagated to the host, root and uid-1000 pods sharing one volume.
  Multi-node behaviour is untested.

## Build

```sh
make build   # local binary, both --mode=controller and --mode=node
make image   # container image (embeds the official retyc/retyc-cli image + davfs2)
```

## Health

Both modes serve `/healthz` (liveness: process up) and `/readyz` (readiness) on `--http-endpoint`
(default `:9808`); CSI `Probe` reports the same readiness. Node: the supervised
`retyc webdav serve` answers on loopback (its last output line is in the error when it doesn't,
e.g. `key passphrase check failed`). Controller: `retyc auth status` is authenticated (cached
2 min). Liveness is deliberately lax on the node — restarting the plugin kills every davfs2 mount
on that node — so a bad credential shows up as a `1/2` NotReady pod, with the reason on `/readyz` and in the
container logs, not as a restart loop.

## Deploy

```sh
cp deploy/secret.yaml.example deploy/secret.yaml
# edit deploy/secret.yaml: RETYC_TOKEN (retyc auth login --offline) + RETYC_KEY_PASSPHRASE
kubectl apply -f deploy/secret.yaml
make deploy
```

Then create a `PersistentVolumeClaim` with `accessModes: [ReadWriteMany]` and one of the two
StorageClasses:

| StorageClass | reclaimPolicy | Deleting the PVC… |
|--------------|---------------|-------------------|
| `retyc-rwx` | `Delete` | deletes the dataroom (`DeleteVolume`) |
| `retyc-rwx-retain` | `Retain` | keeps the dataroom; the PV stays `Released` until you delete it, the dataroom until you `retyc dataroom rm` it |

`deploy/examples/rwx-test.yaml` has a claim plus a writer and a reader pod;
`deploy/examples/static-pv.yaml` shows how to re-adopt an existing dataroom (retained or created
by hand) as a static PV.

## Testing

[`doc/testing.md`](doc/testing.md): staged plan from a manual WebDAV+davfs2 check up to a full
single-node k3s cluster, with what to measure at each step. The cluster is a Debian trixie VM
built by the `Vagrantfile` (libvirt/KVM) and driven through `make vm-up`, `vm-load`, `vm-secret`,
`vm-deploy`.
