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
  can't be applied to a WebDAV/FUSE mount, so this is how non-root pods get write access.
- **A node-plugin pod restart kills every davfs2 mount on that node.** The FUSE daemons live in
  the plugin container; pods then see "Transport endpoint is not connected" until they are
  rescheduled/remounted (the driver does clean up the corrupted mounts on the next
  stage/unstage). Production FUSE-based CSI drivers work around this by running the daemon on
  the host — out of scope for this POC.
- **`CreateVolume` idempotency only sees the first page of `retyc dataroom ls`** (the CLI has no
  `--page` flag); a retried create past that page can produce a duplicate dataroom. Logged as a
  warning when it becomes possible.
- Manual `mount.davfs` validation against the loopback webdav server was attempted on a dev
  laptop and crashed (SIGABRT) inside its sandboxing — almost certainly an environment
  restriction (fuse/mount capability), not a davfs2 defect, but **this needs to be re-validated
  on a real Linux node/VM** before trusting the mount path end-to-end (see plan, verification
  step 1).

## Build

```sh
make build   # local binary, both --mode=controller and --mode=node
make image   # container image (embeds the official retyc/retyc-cli image + davfs2)
```

## Deploy

```sh
cp deploy/secret.yaml.example deploy/secret.yaml
# edit deploy/secret.yaml: RETYC_TOKEN (retyc auth login --offline) + RETYC_KEY_PASSPHRASE
kubectl apply -f deploy/secret.yaml
make deploy
```

Then create a `PersistentVolumeClaim` with `storageClassName: retyc-rwx` and
`accessModes: [ReadWriteMany]` — `deploy/examples/rwx-test.yaml` has one plus a writer and a
reader pod.

## Testing

[`doc/testing.md`](doc/testing.md): staged plan from a manual WebDAV+davfs2 check up to a full
single-node k3s cluster, with what to measure at each step. The cluster is a Debian trixie VM
built by the `Vagrantfile` (libvirt/KVM) and driven through `make vm-up`, `vm-load`, `vm-secret`,
`vm-deploy`.
