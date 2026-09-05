# Architecture

`retyc-k8s-csi` is a filesystem CSI driver in the same family as `csi-driver-nfs` or `csi-driver-smb`: no
attach/detach step, no block devices, one shared filesystem per volume mounted on every node that needs it. The
filesystem is a Retyc dataroom, reached through the WebDAV server built into `retyc-cli` and mounted with `davfs2`.

## Components

| Component | Where | Role |
|-----------|-------|------|
| `retyc-k8s-csi --mode=controller` | `Deployment` in `kube-system`, one replica | `CreateVolume`, `DeleteVolume`, `ValidateVolumeCapabilities` |
| `csi-provisioner` | sidecar of the controller | watches PVCs, drives the controller over the CSI socket |
| `retyc-k8s-csi --mode=node` | `DaemonSet` in `kube-system`, privileged | `NodeStageVolume`, `NodePublishVolume` and their inverses; supervises `retyc webdav serve` |
| `csi-node-driver-registrar` | sidecar of the node plugin | registers the driver socket with kubelet |
| `retyc` | embedded in the image, from `retyc/retyc-cli` | every interaction with the Retyc API and all cryptography |

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
server has its own loopback port and its own `HOME`/`XDG_*` directory under `/var/lib/retyc-csi`, so tokens and caches
never mix between accounts.

## Volume lifecycle

1. **Provisioning.** A PVC with `storageClassName: retyc-rwx` (or `retyc-rwx-retain`) makes `csi-provisioner` call
   `CreateVolume`. The controller runs `retyc dataroom create --title <pv-name>`; the dataroom ID becomes the CSI
   volume handle and the title is stored in the volume context. The call is idempotent: a dataroom already titled
   `<pv-name>` is reused.
2. **Staging.** When a pod using the claim lands on a node, kubelet calls `NodeStageVolume`. The plugin picks the
   WebDAV server of the volume's identity (starting it if needed), waits for it (bounded, 30 s), then runs
   `mount -t davfs -o dir_mode=0777,file_mode=0666 http://127.0.0.1:<port>/dataroom/<title> <globalmount>`.
   One davfs2 mount per volume per node, whatever the number of pods.
3. **Publishing.** `NodePublishVolume` bind-mounts the staging path into the pod's volume directory. With
   `mountPropagation: Bidirectional` on the kubelet directories, the mount made inside the plugin container is visible
   to kubelet and to the pod.
4. **Unpublish / unstage.** The bind mount is removed when the pod goes away; the davfs2 mount when the last pod on the
   node has left. A stale mount (`ENOTCONN` after a plugin restart) is cleaned up here.
5. **Deletion.** With `reclaimPolicy: Delete`, deleting the PVC makes the provisioner call `DeleteVolume`, i.e.
   `retyc dataroom rm retyc://<id> -y`. A dataroom already gone is treated as success. With `Retain`, the PV is
   released and the dataroom kept; `examples/static-pv.yaml` shows how to adopt it again.

## The WebDAV path

`retyc webdav serve` exposes every dataroom its identity can see under `/dataroom/<title>`, decrypting on read and
encrypting on write with the account's AGE identity. Each server listens on a loopback port (`8888` for the default
identity, the next free ones for tenants) inside the node plugin's network namespace, without WebDAV authentication:
the only client that can reach it is the `davfs2` mount started from the same container. Nothing on the node or in
other pods can connect to it.

The supervisor restarts a server five seconds after any exit and keeps its last output line, which is what `/readyz`
reports when a server is down (for instance `key passphrase check failed: wrong key passphrase`), prefixed by the
identity's key.

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
| `retyc webdav serve` exits | node `/readyz` fails, new mounts wait up to 30 s then fail `Unavailable`; existing mounts recover on the next access | automatic, supervisor restart every 5 s |
| Node plugin pod restarts | every davfs2 mount on the node breaks (`Transport endpoint is not connected`) | reschedule the pods; stale mounts are cleaned up at the next stage/unstage |
| Wrong token or passphrase (default identity) | node and controller pods go `1/2` NotReady with the reason on `/readyz`; a controller rollout keeps the previous pod | fix the Secret, restart the DaemonSet/Deployment |
| Wrong token or passphrase (tenant Secret) | that tenant's claims fail to provision or stage; the node pod goes `1/2` while its server crash-loops, other tenants keep working | fix the tenant's Secret; kubelet retries |
| Tenant Secret deleted before its claims | `DeleteVolume` cannot authenticate (with a `Delete` class); `Retain` classes are unaffected | recreate the Secret, or delete the PV by hand |
| Volume mounted within 60 s of its creation | `mount.davfs` gets `404 Not Found` while the server's dataroom-title cache is stale | automatic, kubelet retries until the cache expires |
| Retyc API unreachable | `CreateVolume`/`DeleteVolume` fail and are retried by the provisioner; reads of uncached files fail | automatic |
