# Architecture

`retyc-k8s-csi` is a filesystem CSI driver in the same family as `csi-driver-nfs` or `csi-driver-smb`: no
attach/detach step, no block devices, one shared filesystem per volume mounted on every node that needs it. The
filesystem is a Retyc dataroom, reached through the WebDAV server built into `retyc-cli` and mounted with `davfs2`.

## Overview

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
team ships, embedded from the official `ghcr.io/retyc/retyc-cli` image.

## Components

| Component | Where | Role |
|-----------|-------|------|
| `retyc-k8s-csi --mode=controller` | `Deployment` in `kube-system`, one replica | `CreateVolume`, `DeleteVolume`, `ValidateVolumeCapabilities` |
| `csi-provisioner` | sidecar of the controller | watches PVCs, drives the controller over the CSI socket |
| `retyc-k8s-csi --mode=node` | `DaemonSet` in `kube-system`, privileged | `NodeStageVolume`, `NodePublishVolume` and their inverses; supervises `retyc webdav serve` |
| `csi-node-driver-registrar` | sidecar of the node plugin | registers the driver socket with kubelet |
| `retyc` | embedded in the image, from `ghcr.io/retyc/retyc-cli` | every interaction with the Retyc API and all cryptography |

Both modes are the same binary and the same image. The driver itself never calls the Retyc API: the controller runs
`retyc --json dataroom create|rm|ls` as short-lived subprocesses, the node plugin keeps one long-lived
`retyc webdav serve` per identity in use on the node.

## Identities

An identity is an offline token plus the passphrase of the account's AGE key. The driver knows two sources:

- the **default identity**, from the driver pods' environment (the `retyc-csi-credentials` Secret), used by requests
  that carry no secrets. Optional;
- **per-request secrets**, resolved by external-provisioner and kubelet from a StorageClass's
  `csi.storage.k8s.io/*-secret-*` parameters (`retyc-rwx-tenant`) and handed to `CreateVolume`, `DeleteVolume` and
  `NodeStageVolume`.

The controller runs each `retyc` subprocess with the request's credentials. The node plugin keeps a pool of WebDAV
servers keyed by a hash of the credentials: the default one is pinned for the life of the process, a tenant's is
started at the first `NodeStageVolume` that needs it and stopped at the `NodeUnstageVolume` of its last volume. Each
server has its own two loopback ports (WebDAV and probes) and its own `HOME`/`XDG_*`/`RETYC_CONFIG_DIR` directory
under `/var/lib/retyc-csi`, so tokens and caches never mix between accounts. The CLI's kernel keyring cache of the
unlocked key is turned off (`RETYC_KEYRING_ENABLED=false`) for every subprocess of both components: it lives under one
fixed name in a keyring they all share.

## Volume lifecycle

1. **Provisioning.** A PVC with `storageClassName: retyc-rwx` (or `retyc-rwx-retain`) makes `csi-provisioner` call
   `CreateVolume`. The controller runs `retyc dataroom create --title <pv-name>`; the dataroom ID becomes the CSI
   volume handle and the title is stored in the volume context. The call is idempotent: a dataroom already titled
   `<pv-name>` is reused.
2. **Staging.** When a pod using the claim lands on a node, kubelet calls `NodeStageVolume`. The plugin picks the
   WebDAV server of the volume's identity (starting it if needed), waits for it (bounded, 30 s), then runs
   `mount -t davfs -o dir_mode=0777,file_mode=0666 http://127.0.0.1:<port>/dataroom/<title> <globalmount>`.
   One davfs2 mount per volume per node, whatever the number of pods. The plugin then records the volume on the
   host (see [Recovery](#recovery)).
3. **Publishing.** `NodePublishVolume` bind-mounts the staging path into the pod's volume directory. With
   `mountPropagation: Bidirectional` on the kubelet directories, the mount made inside the plugin container is visible
   to kubelet and to the pod.
4. **Unpublish / unstage.** The bind mount is removed when the pod goes away; the davfs2 mount when the last pod on the
   node has left, along with its record. Mounts that recovery stacked on a target path are all removed.
5. **Deletion.** With `reclaimPolicy: Delete`, deleting the PVC makes the provisioner call `DeleteVolume`, i.e.
   `retyc dataroom rm retyc://<id> -y`. A dataroom already gone is treated as success. With `Retain`, the PV is
   released and the dataroom kept; `examples/static-pv.yaml` shows how to adopt it again.

## The WebDAV path

`retyc webdav serve` exposes every dataroom its identity can see under `/dataroom/<title>`, decrypting on read and
encrypting on write with the account's AGE identity. Each server listens on a loopback port (`8888` for the default
identity, the next free ones for tenants) inside the node plugin's network namespace, without WebDAV authentication:
the only client that can reach it is the `davfs2` mount started from the same container. Nothing on the node or in
other pods can connect to it.

Each server also gets a second loopback port for `--metrics-addr` (`8889` for the default identity). The driver
probes its `/readyz` - it never calls the Retyc API - to decide when a staged volume can be mounted and whether the
node plugin is ready: the listener comes up once the login check and the key unlock succeeded, answers `200` while the
WebDAV port serves, and `503` as soon as the server shuts down (signal, expired login). The same listener serves the
server's `/metrics`, which the node plugin merges, labeled with the identity key and the tenant's namespace, into its own `/metrics`
(see [configuration.md](configuration.md#metrics)).

The supervisor restarts a server after it exits, with an exponential backoff: 5 s, then 10 s, 20 s... up to 5 min,
plus up to 10% of jitter. The backoff starts over once the server was seen serving, or when it had run for a minute
before exiting. Exit codes `77` (login needs a new token: missing, expired or revoked) and `78` (key passphrase missing
or wrong) mean the same credentials will fail again: after three such exits in a row the supervisor gives up on the
server, which stays down until its credentials change (a new identity key, hence a new server) or the node plugin
restarts. Any other exit code, `1` included, may be transient (Retyc API or identity provider unreachable) and is
retried forever. The supervisor keeps the server's last output line, which is what `/readyz` reports when a server is
down (for instance `key passphrase check failed: wrong key passphrase`), prefixed by the identity's key.

`davfs2` is configured for a non-interactive, multi-client mount (`/etc/davfs2/davfs2.conf` in the image):

| Setting | Value | Why |
|---------|-------|-----|
| `ask_auth` | 0 | no TTY to prompt on |
| `use_locks` | 0 | WebDAV locks would be per node and only add round-trips |
| `delay_upload` | 0 | a closed file is uploaded immediately, so other nodes see it sooner |
| `dir_refresh` / `file_refresh` | 5 / 1 s | short listing cache, on top of the CLI's own 30 s cache |

## Consistency

A dataroom is a versioned object store. Through davfs2 it behaves as an eventually consistent shared filesystem:

- a write on node A reaches node B after A's upload, B's listing-cache expiry (up to 30 s in the CLI) and B's
  `dir_refresh`;
- a PUT on an existing name creates a new version server-side; the previous one remains in the dataroom history;
- there is no locking across nodes; concurrent appends to one file interleave or lose lines, last upload wins;
- changes made outside the cluster (web app, another client) appear within about a minute.

## Permissions

Kubernetes `fsGroup` cannot be applied to a FUSE mount, so the driver declares `fsGroupPolicy: None` and mounts with
`dir_mode=0777,file_mode=0666`. Two davfs2 behaviours matter:

- those modes apply to entries davfs2 discovers on the server; a file created through the mount keeps the mode the
  kernel derived from the creating process's umask (0644 for a root pod), until the plugin's metadata cache is
  rebuilt;
- davfs2 enforces permissions itself, and for any uid other than root and the mount owner it starts with
  `getpwuid()` *inside the plugin container*. The image ships `libnss-unknown` so that any `runAsUser` resolves.

## Resource profile

Steady state is about 40 MiB per identity served on the node. Resolving a dataroom session in `retyc webdav serve`
unlocks the account's AGE key with scrypt (work factor 2^18, ~256 MiB of working memory), once per dataroom per
server; the Go runtime returns that memory within minutes. The manifests set a 512 MiB limit for that reason: a lower
limit would OOM-kill the plugin on the first mount, and with it every mount on the node. Raise it with the number of
tenant identities expected per node.

## Failure modes

| Event | Effect | Recovery |
|-------|--------|----------|
| `retyc webdav serve` exits | node `/readyz` fails, new mounts wait up to 30 s then fail `Unavailable`; existing mounts recover on the next access | automatic, supervisor restart with a backoff from 5 s to 5 min |
| Node plugin pod restarts | every davfs2 mount on the node dies with the container; writes in flight are lost | automatic at startup (see [Recovery](#recovery)); pods without `mountPropagation: HostToContainer` need a container restart |
| A `mount.davfs` daemon dies (OOM kill) | that volume answers `Transport endpoint is not connected` on the node | automatic within 30 s, or at the next `NodePublishVolume` of the volume on the node |
| Wrong token or passphrase (default identity) | node and controller pods go `1/2` NotReady with the reason on `/readyz`; the supervisor gives up after three attempts; a controller rollout keeps the previous pod | fix the Secret, restart the DaemonSet/Deployment |
| Wrong token or passphrase (tenant Secret) | that tenant's claims fail to provision or stage; the node pod goes `1/2` while its server is down, other tenants keep working | fix the tenant's Secret; kubelet retries |
| Tenant Secret deleted before its claims | `DeleteVolume` cannot authenticate (with a `Delete` class); `Retain` classes are unaffected | recreate the Secret, or delete the PV by hand |
| Volume mounted within 60 s of its creation | `mount.davfs` gets `404 Not Found` while the server's dataroom-title cache is stale | automatic, kubelet retries until the cache expires |
| Retyc API unreachable | `CreateVolume`/`DeleteVolume` fail and are retried by the provisioner; reads of uncached files fail | automatic |

## Recovery

The davfs2 daemons run in the node plugin container, so a restart of the plugin kills every mount on the node, and
kubelet never calls `NodeStageVolume` or `NodePublishVolume` again for a volume it considers staged. The plugin
repairs those mounts itself:

1. `NodeStageVolume` writes a record per staging path under `--stage-dir` (`/csi/staged`, i.e.
   `<kubeletDir>/plugins/csi.retyc.com/staged` on the host): dataroom title, claim namespace, the major:minor of the
   davfs2 mount and, for a tenant volume, the tenant's credentials. `NodeUnstageVolume` deletes it.
2. At startup, before it opens the CSI socket and while `/readyz` reports `remounting the volumes staged before the
   restart`, the plugin goes through the records: for each staging path whose mount is dead, it waits for the WebDAV
   server of the volume's identity, mounts the dataroom again, and **stacks** a bind mount of the new mount on every
   pod target path still bound to the dead one (found in `/proc/self/mountinfo` by device and source). A DaemonSet
   rollout therefore waits for a node's volumes to be back before moving on to the next node.
3. The same pass runs every 30 s, for a `mount.davfs` killed while the plugin keeps running, and `NodePublishVolume`
   runs it for its volume when a new pod lands on a node whose staging mount is dead.

Stacking rather than replacing is what reaches running pods: a container's mount of the volume is a copy of the target
path's mount, and a mount stacked on the target propagates into that copy when the pod mounts the volume with
`mountPropagation: HostToContainer`, as the examples do. The stacked layers are all unmounted by
`NodeUnpublishVolume`.

`HostToContainer` is recommended, not required. It is Linux `rslave` propagation: mounts made on the host under the
volume's target path show up in the container, nothing propagates back. It needs neither `privileged` nor any
capability (only `Bidirectional` does), and its scope is the claim's own mount point, where only the node plugin,
already privileged, mounts anything: the pod gains no access to the host. Policies that reject any `mountPropagation`
other than `None` usually target `Bidirectional`, or `hostPath` volumes, where it would expose host mounts. Where it is
not allowed anyway, the pod keeps the dead mount until its container restarts and picks up the new one then: give it
a liveness probe that writes to the volume, for instance

```yaml
livenessProbe:
  exec:
    command: ["sh", "-c", "touch /data/.liveness"]
  periodSeconds: 30
  timeoutSeconds: 10
  failureThreshold: 2
```

Recovery does not save data: what a pod was writing when the daemon died, and what davfs2 had not uploaded yet, is lost.
Volumes staged by a driver version without records are not recovered: reschedule their pods once.

The image runs the driver under `tini`, which reaps the detached `mount.davfs` daemons; a killed daemon left as a
zombie would keep its PID file, and `mount.davfs` refuses to mount over it.

## Security

- **Encryption**: files and metadata are encrypted on the node with [AGE](https://github.com/FiloSottile/age)
  post-quantum hybrid keys by `retyc-cli`, before anything leaves the node. Retyc servers only ever see ciphertext.
- **Identity**: a Retyc account per cluster (the driver's `Secret`) or per namespace (`retyc-rwx-tenant`, resolved by
  kubelet and the provisioner from the claim's namespace). Tokens and passphrases only ever live in those Secrets and in
  the environment of the `retyc` processes, plus, for each tenant volume staged on a node, its
  [recovery](#recovery) record (`0600`, root, under `<kubeletDir>/plugins/csi.retyc.com/staged`, deleted at unstage);
  each identity's WebDAV server runs with its own state directory.
- **Trust boundary**: the WebDAV server listens on loopback inside the node plugin's own network namespace, and only the
  `davfs2` mount in that same container talks to it. It is unreachable from pods and from the node.
- **Pod access**: mounts are world-writable (`dir_mode=0777,file_mode=0666`) so that non-root pods can write - Kubernetes
  `fsGroup` cannot be applied to a FUSE mount. Isolation between workloads is therefore at the claim level, not the
  file level: one dataroom per team or application, not per user.

## Limitations

- **No resize, no snapshots, no capacity enforcement.** Requested sizes are accepted and echoed back; Retyc does not cap
  a dataroom's size.
- **A node plugin restart interrupts the mounts on that node.** The FUSE daemons live in the plugin container. The
  driver remounts them at startup (see [Recovery](#recovery)), but writes in flight are lost, and pods without
  `mountPropagation: HostToContainer` only see the volume again after a container restart. Plan node plugin upgrades
  accordingly.
- **File modes are set at creation.** `dir_mode`/`file_mode` apply to entries discovered on the server; a file created
  through the mount keeps the mode derived from the creating pod's umask until the plugin's metadata cache is rebuilt.
- **`CreateVolume` idempotency checks the first page of `retyc dataroom ls` only.** A retried create past that page can
  produce a duplicate dataroom; the driver logs a warning when it becomes possible.
