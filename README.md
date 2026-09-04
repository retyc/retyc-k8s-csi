<p align="center"><img width="200" src=".media/Retyc_Logo_Blue.png" alt="Retyc logo" /></p>

<p align="center">
  <a href="https://kubernetes-csi.github.io/docs/"><img src="https://img.shields.io/badge/CSI-1.11-326ce5.svg" alt="CSI 1.11" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/go-1.26-00ADD8.svg" alt="Go 1.26" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License: MIT" /></a>
</p>

# Retyc CSI Driver

> Kubernetes CSI driver for [Retyc](https://retyc.com) datarooms - `ReadWriteMany` persistent volumes with end-to-end
> post-quantum encryption, shared between pods and nodes, with the data staying in Europe.

---

## What is Retyc?

[Retyc](https://retyc.com) is a European sovereign file-sharing platform with end-to-end post-quantum encryption. Data
stays in Europe, GDPR-compliant by design.

`retyc-k8s-csi` turns Retyc datarooms into Kubernetes volumes: one `PersistentVolumeClaim` is one dataroom, mounted on
every node that needs it. Files are encrypted and decrypted on the node by the official
[`retyc-cli`](https://github.com/retyc/retyc-cli); the Retyc servers never see plaintext.

---

## How it works

```
   PersistentVolumeClaim                                 Pod
          │                                               │ /data
          ▼                                               ▼
  ┌─ controller ──────────┐              ┌─ node plugin (DaemonSet) ────────────────────┐
  │ csi-provisioner       │              │ node-driver-registrar                        │
  │ retyc-k8s-csi         │              │ retyc-k8s-csi ── mount -t davfs ──▶ davfs2   │
  │   └─ retyc dataroom   │              │   └─ retyc webdav serve ◀── loopback ────┘   │
  │      create / rm      │              └──────────────┬───────────────────────────────┘
  └──────────┬────────────┘                             │  encrypted chunks
             ▼                                          ▼
                              Retyc API
```

- **Controller** (`--mode=controller`, one replica): creates a dataroom per `CreateVolume`, deletes it on `DeleteVolume`.
- **Node plugin** (`--mode=node`, one per node): supervises a local `retyc webdav serve`, mounts each dataroom with
  `davfs2` on a per-volume staging path, then bind-mounts it into every pod that uses the claim.
- **Sidecars**: the standard `csi-provisioner` and `csi-node-driver-registrar` from kubernetes-csi.

The driver never speaks to the Retyc API itself: every operation goes through the same vetted `retyc` binary the CLI
team ships, embedded from the official `retyc/retyc-cli` image. Full walkthrough: [doc/architecture.md](doc/architecture.md).

---

## Requirements

- Kubernetes 1.25+ with a kubelet that allows privileged pods and `mountPropagation: Bidirectional` on the nodes
- `/dev/fuse` on every node (davfs2 is a FUSE filesystem)
- A Retyc account, an offline token (`retyc auth login --offline`) and the passphrase of its AGE key
- Outbound HTTPS from the nodes to `api.retyc.com`

---

## Installation

### With Helm (recommended)

```sh
read -rs RETYC_KEY_PASSPHRASE          # never on the command line history
helm install retyc-csi ./charts/retyc-csi \
  --namespace kube-system \
  --set credentials.token="$RETYC_TOKEN" \
  --set credentials.keyPassphrase="$RETYC_KEY_PASSPHRASE"

kubectl -n kube-system get pods -l app.kubernetes.io/instance=retyc-csi   # 2/2 Running
```

The chart installs the `CSIDriver`, RBAC, the controller `Deployment`, the node `DaemonSet` and the three
StorageClasses. `credentials.existingSecret` points it at a Secret you manage yourself; without any credentials the
driver serves per-namespace identities only. Every value is documented in
[charts/retyc-csi/README.md](charts/retyc-csi/README.md).

### With plain manifests

`deploy/` holds the same resources as raw YAML, used by the development loop:

```sh
cp deploy/secret.yaml.example deploy/secret.yaml     # RETYC_TOKEN + RETYC_KEY_PASSPHRASE
kubectl apply -f deploy/secret.yaml
make deploy
```

### Container image

The image bundles the driver, `davfs2` and the `retyc` CLI. Build and push it to your registry:

```sh
make image                              # retyc/retyc-k8s-csi:dev
make image RETYC_VERSION=v1.2.0         # pin the embedded retyc-cli release
```

---

## Quick start

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: shared-data
spec:
  accessModes: [ReadWriteMany]
  storageClassName: retyc-rwx
  resources:
    requests:
      storage: 1Gi
```

Every pod mounting `shared-data` - on any node - reads and writes the same dataroom. The dataroom appears in
`retyc dataroom ls` and in the web app under the name of the `PersistentVolume`.

| StorageClass       | Identity                                  | reclaimPolicy | Deleting the PVC                                          |
|--------------------|-------------------------------------------|---------------|-----------------------------------------------------------|
| `retyc-rwx`        | cluster-wide (driver Secret)              | `Delete`      | deletes the dataroom                                      |
| `retyc-rwx-retain` | cluster-wide (driver Secret)              | `Retain`      | keeps the dataroom; the PV stays `Released` until deleted |
| `retyc-rwx-tenant` | the namespace's own `retyc-credentials`   | `Retain`      | keeps the dataroom, in the namespace's account            |

An existing dataroom - retained or created by hand - can be adopted as a static volume: see
[deploy/examples/static-pv.yaml](deploy/examples/static-pv.yaml). A complete writer/reader example lives in
[deploy/examples/rwx-test.yaml](deploy/examples/rwx-test.yaml).

### Multi-tenant

With `retyc-rwx-tenant`, each namespace brings its own Retyc account: a Secret named `retyc-credentials` in the
namespace, holding `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE`. Datarooms are created in that account and mounted with
it, by a dedicated WebDAV server on each node. The driver's own Secret is optional in that setup. Example:
[deploy/examples/tenant.yaml.example](deploy/examples/tenant.yaml.example).

---

## Security

- **Encryption**: files and metadata are encrypted on the node with [AGE](https://github.com/FiloSottile/age)
  post-quantum hybrid keys by `retyc-cli`, before anything leaves the node. Retyc servers only ever see ciphertext.
- **Identity**: a Retyc account per cluster (the driver's `Secret`) or per namespace (`retyc-rwx-tenant`, resolved by
  kubelet and the provisioner from the claim's namespace). Tokens and passphrases only ever live in those Secrets and in
  the environment of the `retyc` processes; each identity's WebDAV server runs with its own state directory.
- **Trust boundary**: the WebDAV server listens on loopback inside the node plugin's own network namespace, and only the
  `davfs2` mount in that same container talks to it. It is unreachable from pods and from the node.
- **Pod access**: mounts are world-writable (`dir_mode=0777,file_mode=0666`) so that non-root pods can write - Kubernetes
  `fsGroup` cannot be applied to a FUSE mount. Isolation between workloads is therefore at the claim level, not the
  file level: one dataroom per team or application, not per user.

---

## Consistency model

Retyc datarooms are an object store with versioning, not a POSIX filesystem. A volume behaves as an
**eventually consistent** shared filesystem:

- a file written on one node is visible on the others after a few seconds (davfs2 refresh + the CLI's listing cache);
- overwriting a file creates a new version server-side;
- there is no cross-node locking - concurrent writers to the same file lose lines, last upload wins.

This suits shared configuration, build artefacts, documents and hand-offs between jobs. It does not suit databases,
write-ahead logs or any workload that appends to one file from several places.

---

## Health

Both modes serve `/healthz` (liveness) and `/readyz` (readiness) on `--http-endpoint` (default `:9808`), and the CSI
`Probe` RPC reports the same readiness.

- **Node**: ready when the supervised `retyc webdav serve` answers on loopback. When it does not, `/readyz` returns
  the process state and its last output line - e.g. `key passphrase check failed` - and the same line is logged.
- **Controller**: ready when `retyc auth status` reports an authenticated account (checked every two minutes).

Liveness is deliberately lenient on the node: restarting the plugin breaks every mount on that node, so a bad
credential shows up as a `1/2` NotReady pod, never as a restart loop. See
[doc/troubleshooting.md](doc/troubleshooting.md).

---

## Limitations

- **No resize, no snapshots, no capacity enforcement.** Requested sizes are accepted and echoed back; Retyc does not cap
  a dataroom's size.
- **A node plugin restart breaks the mounts on that node.** The FUSE daemons live in the plugin container. Pods see
  `Transport endpoint is not connected` until they are rescheduled; the driver cleans the stale mounts up on the next
  stage/unstage. Plan node plugin upgrades like a node drain.
- **File modes are set at creation.** `dir_mode`/`file_mode` apply to entries discovered on the server; a file created
  through the mount keeps the mode derived from the creating pod's umask until the plugin's metadata cache is rebuilt.
- **`CreateVolume` idempotency checks the first page of `retyc dataroom ls` only.** A retried create past that page can
  produce a duplicate dataroom; the driver logs a warning when it becomes possible.

---

## Documentation

| Topic                          | Link                                             |
|--------------------------------|--------------------------------------------------|
| Architecture & volume lifecycle | [doc/architecture.md](doc/architecture.md)       |
| Configuration (flags, Secret, StorageClasses, probes) | [doc/configuration.md](doc/configuration.md) |
| Helm chart values              | [charts/retyc-csi/README.md](charts/retyc-csi/README.md) |
| Troubleshooting                | [doc/troubleshooting.md](doc/troubleshooting.md) |
| Testing (unit, csi-sanity, k3s VM) | [doc/testing.md](doc/testing.md)             |

---

## Development

```sh
make build        # local binary, both modes
make test         # go test -race ./...
make lint         # golangci-lint, same rules as retyc-cli
make helm-lint    # chart lint + render in every credential mode
make image        # container image

# Full end-to-end environment: single-node k3s on Debian trixie, Vagrant + libvirt
make vm-up        # boot + provision, then: make image vm-load vm-secret vm-deploy
```

---

## License

[MIT](LICENSE) - © Retyc / TripleStack SAS
