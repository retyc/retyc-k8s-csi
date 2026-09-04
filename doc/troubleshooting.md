# Troubleshooting

Start with the state of the two driver pods and their readiness:

```sh
kubectl -n kube-system get pods -l 'app in (retyc-csi-controller, retyc-csi-node)'
kubectl -n kube-system logs ds/retyc-csi-node -c retyc-csi-node
kubectl -n kube-system logs deploy/retyc-csi-controller -c retyc-csi-controller
```

A driver container that refuses to be ready logs the reason on every failed probe (`/readyz: not ready: ...`); kubelet's
own event only carries the HTTP 503.

## Node pod is `1/2`, `/readyz` says `retyc webdav serve is not running`

The supervised WebDAV server exits at start and is restarted every five seconds. The error includes its last output
line:

| Last output | Cause |
|-------------|-------|
| `key passphrase check failed: wrong key passphrase` | `RETYC_KEY_PASSPHRASE` in the Secret is wrong |
| `not authenticated, run retyc auth login: no stored token` | `RETYC_TOKEN` is missing, expired or revoked |
| `warning: RETYC_TOKEN is expired or revoked` | same, the CLI then tries and fails on a disk token that does not exist |

Fix the Secret, then `kubectl -n kube-system rollout restart ds/retyc-csi-node`.

## Controller pod is `1/2`

`retyc auth status` does not report an authenticated account: token problem, same fix as above with
`rollout restart deploy/retyc-csi-controller`. Until then the previous controller pod, if any, keeps serving.

## Pod stuck in `ContainerCreating`, `MountVolume.MountDevice failed ... Unavailable: local webdav server not ready`

`NodeStageVolume` waited 30 s for the local WebDAV server. Look at the node pod's readiness (above). Once the server is
back, kubelet's next retry succeeds without touching the pod.

## `Transport endpoint is not connected` inside a pod

The node plugin restarted on that node (upgrade, OOM kill, crash) and took the FUSE daemons with it. Reschedule the
affected pods; the driver removes the stale mounts when it stages or unstages next. Check `kubectl -n kube-system get
pods -l app=retyc-csi-node -o wide` for `RESTARTS` and, if the cause is an OOM kill, the memory limit
(see [configuration.md](configuration.md#probes-and-resources)).

## `mount.davfs: can't access file /etc/mtab`

The image is missing the `/etc/mtab -> /proc/mounts` symlink. Docker creates it at container start, containerd does
not, so the image must ship it (the `Dockerfile` does). Rebuild from a clean checkout.

## `Permission denied` for a non-root pod on a `0777` directory

davfs2 enforces permissions itself and calls `getpwuid()` for the pod's uid inside the plugin container. The image ships
`libnss-unknown` so any uid resolves; without it every non-root uid is denied regardless of the mode bits. Verify with:

```sh
kubectl -n kube-system exec ds/retyc-csi-node -c retyc-csi-node -- getent passwd 1000
```

## `group davfs2 does not exist` at mount time

`libnss-unknown` was active while the `davfs2` package was configured, so its postinst believed the account already
existed and skipped creating it. The `Dockerfile` installs the two packages in separate steps for this reason.

## PV stays `Released` with `Delete` policy, provisioner logs `cannot patch resource "persistentvolumes"`

The provisioner's ClusterRole lacks `patch` on `persistentvolumes`, which it needs to remove its finalizer. Apply the
current `deploy/rbac.yaml`.

## Writes from one node take long to appear on another

Expected up to about 30 s plus `dir_refresh`: the CLI caches directory listings and a node only learns of another
node's upload when its own cache expires. Changes made outside the cluster take up to a minute. See
[architecture.md](architecture.md#consistency).

## Memory spikes to ~350 MiB on the node plugin

Unlocking the AGE key runs scrypt with ~256 MiB of working memory, once per dataroom per WebDAV server process. It is
transient; the runtime returns the memory within minutes. Only a limit below that peak is a problem.
