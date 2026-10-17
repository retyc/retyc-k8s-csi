# Testing

Two stages, cheapest first:

| Stage | What it proves | Runs |
|-------|----------------|------|
| 1. Unit tests, lint | The driver's logic against fake Kubernetes clients and a fake `retyc`; the chart renders | on the host |
| 2. End to end on k3s | The whole thing: mount pods, RetycFS, mount propagation, recovery, admission policy, Redis | in a Vagrant VM, driven from the host |

"The VM" is one Debian 13 (trixie) machine running k3s, built by the `Vagrantfile` at the repo root on top of
libvirt/KVM and driven through `make vm-*`. RetycFS mounts, `Bidirectional` propagation and privileged pods need a kernel
you control, which a sandboxed shell or a CI runner is not.

Everything mutating talks to a **real Retyc API** and creates and deletes datarooms: use a test account, and check
`retyc --json user quota` before and after.

## Stage 1: unit tests and lint

On the host:

```sh
make lint         # golangci-lint, same rules as retyc-cli (.golangci.yml)
make test         # go test -race ./...
make helm-lint    # helm dependency build, helm lint, CLI version check, three renders
```

- `internal/mountpod` (pod spec, Secrets, finalizer, backoff, `mount.json`) and `internal/driver` (stage, unstage,
  recovery, the pod watch, the controller) are tested with client-go's fake clientset and a fake mount table.
- `internal/retycclient` is tested against a fake `retyc` script that reproduces the real CLI's stdout and stderr,
  spinner escapes included. When retyc-cli changes its JSON shapes, capture them again and update the fixtures:

  ```sh
  retyc --json dataroom create --title fixture-check --storage-capacity 20GiB --versioning=true
  retyc --json dataroom set retyc://<id> --storage-capacity 30GiB -y
  retyc --json dataroom ls | head -20
  retyc --json user quota
  retyc --json dataroom rm retyc://<id> -y
  retyc --json dataroom rm retyc://00000000-0000-0000-0000-000000000000 -y   # exit 1, a 404 on stderr
  ```

- `make helm-lint` runs `helm dependency build charts/retycfs-csi` first: the Redis subchart is a dependency, even when
  disabled, and `helm template` or `helm install` of the source chart fails without it.
- `.github/scripts/check-cli-version.sh` checks that the Dockerfile's `RETYC_CLI_VERSION` and the chart's
  `mount.image.tag` name the same retyc-cli.

## Stage 2: end to end on k3s

### Prerequisites

On the host:

```sh
# Vagrant + libvirt provider; your user must be in the libvirt group (log out/in after adding).
sudo apt-get install -y vagrant libvirt-daemon-system qemu-kvm
vagrant plugin install vagrant-libvirt
sudo usermod -aG libvirt "$USER"

# retyc-cli, authenticated against the API you test with, and a long-lived offline token:
retyc auth status --json
retyc auth login --offline
```

Then the VM:

```sh
make vm-up        # vagrant up (box download + k3s install on first run, a few minutes), then copies retyc in
make vm-ssh       # a shell inside; kubectl and helm work without sudo there
```

| Target | Does |
|--------|------|
| `make vm-up` | `vagrant up --provider=libvirt`, then `make vm-retyc` |
| `make vm-retyc` | copies the host's `retyc` into the VM (`RETYC_BIN=...` to pick another) |
| `make vm-load` | `docker save` the driver image into k3s' containerd |
| `make cli-image` | builds the development retyc-cli image `CLI_IMAGE` (default `retyc-cli:retycfs-v1.5.0-beta2`) from the tag `CLI_VERSION` (default `v1.5.0-beta2`) of the checkout `CLI_SRC` (default `../retyc_cli`), without `-tags prod` (see below) |
| `make vm-load-cli` | the same as `vm-load` for the retyc-cli image of the mount pods (`CLI_IMAGE=...`, default `retyc-cli:retycfs-v1.5.0-beta2`, as in `examples/vagrant-values.yaml`) |
| `make vm-secret` | creates the `retycfs-csi-credentials` Secret in `kube-system` from `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE` |
| `make vm-helm` | `vagrant rsync`, then `helm upgrade --install` of the chart in `kube-system` with `examples/vagrant-values.yaml` (`VM_VALUES=...` for another file) |
| `make vm-restart` | restarts the controller and the node plugin, needed after a `vm-load` of the same tag |
| `make vm-ssh` / `make vm-destroy` | `vagrant ssh` / `vagrant destroy -f` (the box stays cached) |

A VM provisioned before the Vagrantfile installed `fuse3` has no `fusermount3`: run `vagrant provision` (or
`sudo apt-get install -y fuse3` in the VM).

The VM has 2 vCPUs, 4 GB of RAM, NAT to the internet, `fuse3` with the `fuse` module loaded at boot (`/dev/fuse`
for the mount pods), k3s with its bundled Traefik, Helm, and a Jaeger all-in-one
receiving the CLI's traces (UI on http://localhost:18081). Nothing in the Makefile touches a cluster from the host:
every `vm-*` target runs `kubectl`/`helm` inside the VM over `vagrant ssh`. The repo is rsynced one-way to `/vagrant`
(`vagrant rsync` after edits). The `[fog][WARNING] Unrecognized arguments: libvirt_ip_command` line every vagrant
command prints is vagrant-libvirt 0.12.x noise.

### Against the development API

A retyc-cli release is a `prod` build, which does not honour `RETYC_INSECURE`: the tests run against the development
API, with a CLI built locally without that build tag.

| | Value |
|---|---|
| API | `https://api.triplesfer.traefik.me`, the default of a dev build of the CLI (`RETYC_API_BASE_URL`) |
| CLI image | built from a retyc-cli tag (here `v1.5.0-beta2`) without the `prod` tag, tagged `retyc-cli:retycfs-v1.5.0-beta2` |
| Certificate | `RETYC_INSECURE=true`, which only a dev build of the CLI honours |
| Credentials | an offline token and the key passphrase of a test account, in an untracked file at the repo root (`.env.dev`, mode 0600, excluded from git). **Never in a tracked file.** |

1. **Build a development CLI with a version.** A CLI built without a semantic version is refused by the API with
   `426 Retyc CLI version could not be determined`, and retyc-cli's Dockerfile builds with `-tags prod`, which ignores
   `RETYC_INSECURE`. `make cli-image` builds an export of the tag `CLI_VERSION` from the retyc-cli checkout next to
   this one (`CLI_SRC=...` for another), with a copy of the Dockerfile without that line: the checkout stays
   untouched.

   ```sh
   make cli-image                                              # CLI_VERSION=v1.5.0-beta2
   docker run --rm retyc-cli:retycfs-v1.5.0-beta2 version      # retyc v1.5.0-beta2 (dev build)
   ```

   The command is `retyc version`; `retyc --version` is an unknown flag.

2. **Build the driver** against that local CLI image (the controller embeds its binary), and load both images into
   k3s, which pulls from no local registry:

   ```sh
   make image RETYC_CLI_IMAGE=retyc-cli RETYC_CLI_VERSION=retycfs-v1.5.0-beta2
   make vm-load
   make vm-load-cli
   ```

3. **Credentials and chart.** The dev API's names resolve to `127.0.0.1`, so `examples/vagrant-values.yaml` gives the
   controller and the mount pods `hostAliases` to the host as seen from the VM (`192.168.121.1`) for
   `api.triplesfer.traefik.me` and `auth.triplesfer.traefik.me`, plus `RETYC_API_BASE_URL` and `RETYC_INSECURE` in
   `controller.extraEnv` and `mount.extraEnv`, and `retyc-cli:retycfs-v1.5.0-beta2` as `mount.image`.

   ```sh
   set -a; . ./.env.dev; set +a
   make vm-secret
   helm dependency build charts/retycfs-csi    # the subchart travels with the rsync
   make vm-helm
   ```

4. **Check** in the VM:

   ```sh
   kubectl -n kube-system get pods -l app.kubernetes.io/name=retycfs-csi    # controller 3/3, node 2/2
   ```

The token of `.env.dev` is short-lived. Once it has expired, the controller stays `2/3`: its readiness probe logs
`retyc not authenticated: no_token` (the API refuses the token, and the container has no other login), and no claim is
provisioned. Mint a new one, then `make vm-secret` and `make vm-restart`. Iterating on the driver
is `make image ... vm-load vm-restart`; on the chart, `make vm-helm`.

With a released CLI and the production API, skip the dev settings: `make image`, `make vm-load`, `make vm-secret`,
`make vm-helm` with a values file that only sets `image.tag: dev` and `credentials.existingSecret`.

### A CLI pod in the dev cluster

To call `retyc` with the cluster's credentials (list or remove a dataroom, check the quota) without the host's login,
run a one-shot pod: the Secret is the driver's, `hostAliases` and `RETYC_INSECURE` are the dev API settings.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: retyc-cli
  namespace: kube-system
spec:
  restartPolicy: Never
  hostAliases:
    - ip: 192.168.121.1
      hostnames: [api.triplesfer.traefik.me, auth.triplesfer.traefik.me]
  containers:
    - name: cli
      image: retyc-cli:retycfs-v1.5.0-beta2
      imagePullPolicy: IfNotPresent
      args: [--json, dataroom, ls]
      env:
        - {name: RETYC_API_BASE_URL, value: "https://api.triplesfer.traefik.me"}
        - {name: RETYC_INSECURE, value: "true"}
        - {name: RETYC_KEYRING_ENABLED, value: "false"}
      envFrom:
        - secretRef: {name: retycfs-csi-credentials}
```

```sh
kubectl apply -f retyc-cli.yaml && kubectl -n kube-system logs -f retyc-cli; kubectl -n kube-system delete pod retyc-cli
```

Before the first scenario, list the datarooms the account already has and leave them alone: the tests create and delete
only the `pvc-...` ones, and a leftover from an earlier run is the one to remove by hand.

```sh
retyc --json dataroom ls > /tmp/datarooms-before.json     # (host) keep it; compare at the end
```

`make vm-helm` runs `helm upgrade --install` **without** `--reuse-values`: it puts the release back on
`examples/vagrant-values.yaml`, dropping what the scenarios `--set` (Redis, replicas). Re-apply them after a `vm-helm`.

### Scenarios

In the VM, from `/vagrant`. Mount pods live in `kube-system`; `mp` below lists them:

```sh
alias mp='kubectl -n kube-system get pods -l app.kubernetes.io/name=retycfs-mount -o wide -L csi.retycfs.com/pvc'
```

**1. A claim without `uid` is refused.**

```sh
sed '/annotations:/,/csi.retycfs.com\/uid/d' examples/rwx-test.yaml | kubectl apply -f -
```

Expected: `denied request: ValidatingAdmissionPolicy ...: annotation csi.retycfs.com/uid is required`. With
`--set admissionPolicy.enabled=false`, the claim is created but stays `Pending`, with a `ProvisioningFailed` event
`InvalidArgument ... annotation csi.retycfs.com/uid is required`, and no dataroom is created.

**2. A `1Gi` claim gets 20 GiB.**

```sh
kubectl apply -f examples/rwx-test.yaml
kubectl get pvc retycfs-rwx-test -w                 # Bound, CAPACITY 20Gi
retyc --json dataroom ls                          # (host) the pvc-... dataroom, storage capacity 21474836480
```

**3. The owner UID writes, another UID does not.** In `rwx-test.yaml`, the writer runs as 1000 (the claim's `uid`),
the reader as 2000:

```sh
kubectl logs -f retycfs-reader                                    # the writer's lines every 5 s
kubectl exec retycfs-writer -- ls -ln /data                       # owner 1000:1000
kubectl exec retycfs-reader -- touch /data/x                      # Permission denied
```

**4. Two pods, one mount pod.** Both pods are on the single node: one mount pod for the volume, one RetycFS mount on the
staging path, one bind per pod.

```sh
mp                                                              # one retycfs-mount-..., 1/1 Running
findmnt -t fuse.retycfs                                         # source retyc://<id>: globalmount + 2 target paths
```

**5. Stats.** `NodeGetVolumeStats` serves the mount's `statfs`:

```sh
kubectl get --raw "/api/v1/nodes/$(hostname)/proxy/stats/summary" | jq '.pods[].volume[]? | select(.pvcRef.name=="retycfs-rwx-test")'
```

**6. Expand.**

```sh
kubectl patch pvc retycfs-rwx-test -p '{"spec":{"resources":{"requests":{"storage":"30Gi"}}}}'
kubectl get pvc retycfs-rwx-test -w                 # CAPACITY 30Gi, no node step
retyc --json dataroom ls                          # (host) 32212254720
kubectl exec retycfs-writer -- df -h /data          # the new size within a minute (cached dataroom stats)
```

**7. `kill -9` of the mount process.** The volume comes back in about 10 to 18 s, without restarting the pods (they
mount it with `HostToContainer`). The process is `/retyc fs mount ...`, so match on its command line (`-x retyc` finds
nothing):

```sh
sudo pkill -9 -f '^/retyc fs mount'               # every mount process of the node: fine in the VM
mp -w                                             # the pod exits (137), a new one is Running then 1/1
kubectl exec retycfs-writer -- ls /data             # Transport endpoint is not connected, then works again
kubectl -n kube-system logs ds/retycfs-csi-node -c retycfs-csi-node | grep recovery:   # recreated, rebinding
```

A consumer container may restart meanwhile, depending on its command: the writer of `rwx-test.yaml` fails its `date >>`
while the mount is dead and keeps looping, a command that exits on error restarts with its pod's policy.

**8. Node plugin restart.** No mount is cut:

```sh
kubectl -n kube-system delete pod -l app.kubernetes.io/name=retycfs-csi,app.kubernetes.io/component=node
mp                                                # same mount pod, same age
kubectl logs --tail 3 retycfs-reader                # no gap in the writer's lines
```

**9. Unstage.** Deleting the pods unstages the volume: the mount pod drains and is deleted, the staging path is
unmounted.

```sh
kubectl delete pod retycfs-writer retycfs-reader
mp -w                                             # Terminating, then gone
findmnt -t fuse.retycfs                           # nothing
kubectl -n kube-system logs ds/retycfs-csi-node -c retycfs-csi-node | grep -E 'exited cleanly|unstaged'
kubectl -n kube-system get secrets | grep retycfs-mount   # gone with their pods
```

**10. Admission policy.** Each of these is refused with the annotation's expected format:

```sh
kubectl annotate pvc retycfs-rwx-test csi.retycfs.com/uid=abc --overwrite
kubectl annotate pvc retycfs-rwx-test csi.retycfs.com/cache-ttl=-1s
kubectl annotate pvc retycfs-rwx-test csi.retycfs.com/local-file='none *.tmp'
kubectl annotate pvc retycfs-rwx-test csi.retycfs.com/mount-memory-limit=0
kubectl annotate pvc retycfs-rwx-test csi.retycfs.com/uid-                     # removing the uid
```

A valid change (`csi.retycfs.com/uid=2000 --overwrite`) is accepted, and applies at the next mount.

**11. Redis.**

```sh
helm upgrade retycfs-csi /vagrant/charts/retycfs-csi -n kube-system --reuse-values -f /vagrant/examples/redis-values.yaml --wait
kubectl apply -f examples/rwx-test.yaml           # a new stage: the mount pod uses the new cache settings
kubectl -n kube-system get pod -l app.kubernetes.io/name=retycfs-mount -o jsonpath='{.items[0].spec.containers[0].args}'   # --cache-store=redis --cache-redis-high-confidentiality
kubectl -n kube-system exec retycfs-csi-redis-0 -- sh -c 'redis-cli -a "$REDIS_PASSWORD" --no-auth-warning dbsize'
```

The pod and variable names of the Redis subchart may differ between its versions: `kubectl -n kube-system get pods -l
app.kubernetes.io/name=redis`. A volume already mounted before the upgrade keeps its memory cache until it is
remounted (`MountConfigChanged`).

**12. Leader election.**

```sh
helm upgrade retycfs-csi /vagrant/charts/retycfs-csi -n kube-system --reuse-values --set controller.replicas=2 --wait
kubectl -n kube-system get lease | grep csi-retycfs-com   # the provisioner's and the resizer's leases, one holder each
kubectl -n kube-system delete pod <the holder>          # the other replica takes over
kubectl apply -f examples/rwx-test.yaml                 # still provisioned
```

**13. `ReadWriteOncePod`.** A claim with `accessModes: [ReadWriteOncePod]`, mounted by two pods:

```sh
kubectl get events --field-selector involvedObject.name=<second pod>   # FailedScheduling ... ReadWriteOncePod access mode already in-use
kubectl get pod <second pod>                                           # Pending until the first one is gone
```

The second pod stays `Pending` with `FailedScheduling ... node has pod using PersistentVolumeClaim with the same name
and ReadWriteOncePod access mode already in-use`; the mount pod is the first pod's. The first Event of a pod can be a
transient `unbound immediate PersistentVolumeClaims` while the claim is still being provisioned: it goes away once the
claim is `Bound`.

**14. Tenant fail-closed.** A static PV of a tenant class (`retycfs-rwx-tenant`) without `nodeStageSecretRef` must not be
mounted with the driver's global identity. Take `examples/static-pv.yaml`, set `storageClassName: retycfs-rwx-tenant`
and a dataroom ID you created for the test, and leave the `nodeStageSecretRef` block commented out; a consumer pod
then stays in `ContainerCreating`:

```sh
kubectl describe pod <consumer>                  # FailedMount ... FailedPrecondition, refusing the driver's global identity for a tenant volume
mp                                               # no mount pod was created
```

Uncomment `nodeStageSecretRef` with the Secret of `examples/tenant.yaml` (`retycfs-credentials`, in the claim's
namespace): the stage goes through.

**15. Events on the claim.** `MountGaveUp`, `MountFailed`, `WritesLost` and `MountOOMKilled` are Events **of the PVC**, in the PVC's
namespace (`kubectl describe pvc <claim>`, `kubectl get events -n <ns>`), not of the mount pod in `kube-system`.

- `MountGaveUp`: put a **fake** token in the credentials Secret (the real one is never used for this), for instance with
  a tenant Secret in a test namespace (`RETYC_TOKEN=fake`), and start a pod on a claim of that class. The mount pod
  exits `77` three times (5 s, then 10 s apart); the claim gets one `MountGaveUp`, and the next stages fail at once with
  `FailedPrecondition`. Restore the Secret, then see [troubleshooting.md](troubleshooting.md#events-on-the-claim) for the
  end of the give-up.
- `MountFailed` against `WritesLost`: a failure at start writes nothing and must not say otherwise. Provoke one without
  risk with a Redis that does not answer, such as an unrouted address:

  ```sh
  helm upgrade retycfs-csi /vagrant/charts/retycfs-csi -n kube-system --reuse-values \
    --set cache.store=redis --set cache.redis.subchart.enabled=false \
    --set cache.redis.external.url=redis://10.255.255.1:6379
  kubectl apply -f examples/rwx-test.yaml          # the mount pod exits 1 before it is ever ready
  kubectl describe pvc retycfs-rwx-test              # MountFailed ("failed before mounting it (nothing was written)")
  ```

  `WritesLost` is for a pod that exits `75` (or, with a CLI older than that code, `1` with an output that reports lost
  files); it is not provoked on purpose (it needs a cut API during a drain). Put `cache.store=memory` back afterwards.
- `MountOOMKilled`, without writing anything, on a claim in use by a pod that mounts it with
  `mountPropagation: HostToContainer`:
  - **OOM kill**: annotate the claim with `csi.retycfs.com/mount-memory-limit: 200Mi`, under the ~270 MiB the key
    unlocking takes at start, and remount it (restart the pod). The mount pod is killed `137` at each start; the claim
    gets a `MountOOMKilled` "killed at its memory limit (200Mi)" for every kill. Remove the annotation and remount.
    A walk alone (`ls -lR` of 85 000 files at `300Mi`) does not get there: the kernel reclaims, the cgroup stays at
    its limit.
  - **A kill nobody asked for**: `sudo kill -9 $(pgrep -f 'retyc fs mount')` on the node while the volume serves.
    containerd reports `Error`, the claim gets a `MountOOMKilled` "killed (SIGKILL) while serving, most likely at
    its memory limit", the pod is recreated, and the application reads again within ~10 s without a restart (its
    mount was rebound). Without `HostToContainer`, it keeps `Transport endpoint is not connected` until restarted.

### Teardown, and check the dataroom is gone

```sh
kubectl delete -f /vagrant/examples/rwx-test.yaml
kubectl get pv                                  # the released PV disappears (reclaimPolicy: Delete)
retyc --json dataroom ls | grep -c '"pvc-'      # (host) 0: DeleteVolume removed the dataroom
retyc --json user quota                         # (host) count_dataroom back to where it started
```

`vagrant halt` stops the VM and keeps it; `make vm-destroy` starts fresh next time.

## Known gaps

- **csi-sanity** does not run against this driver as is: both modes need an in-cluster Kubernetes client, and
  `CreateVolume` reads the claim named by `csi.storage.k8s.io/pvc/*` parameters. Running it would take a pod in the
  cluster and test parameters naming a real claim.
- **Multi-node**: the VM is a single node. Cross-node visibility (the cache TTL) and several mount pods of one volume
  are not exercised; a second VM joining k3s as an agent would cover them.
- **Drain**: what `kubectl drain` does to the mount pods (see [architecture.md](architecture.md#limitations)), and
  kubelet's timeout on a `NodeUnstageVolume` that returns `Aborted` for a 5 min drain, are not covered.
- **Load and throughput**: every read is a chunk download and an AGE decryption; nothing here measures it.
