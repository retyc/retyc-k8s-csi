# Testing retyc-k8s-csi

Four stages, cheapest first. Each one validates an assumption the next one depends on, so don't
skip straight to the cluster.

| Stage | What it proves | Runs |
|-------|----------------|------|
| 1. Manual WebDAV + davfs2 | The storage path works at all, and how consistent it is | in the VM, as root |
| 2. Unit tests + real CLI | The `retyc` exec wrapper decodes what the real CLI prints | on the host |
| 3. csi-sanity (optional) | gRPC conformance of the Controller/Node services | in the VM, as root |
| 4. Single-node k3s | The whole thing, including kubelet mount propagation and FUSE | in the VM, driven from the host |

"The VM" is one Debian 13 (trixie) machine running k3s, built by the `Vagrantfile` at the repo
root on top of libvirt/KVM and driven through `make vm-*`. Stages 1 and 3 need a kernel you
control (`/dev/fuse`, `CAP_SYS_ADMIN`, real mounts) — `mount.davfs` dies with SIGABRT inside a
sandboxed shell — so build the VM first, even if you only care about stage 1.

Everything mutating talks to a **real Retyc account**: use a test account, and check
`retyc --json user quota` before/after — stages 1, 3 and 4 create and delete datarooms.

## Prerequisites

On the host:

```sh
# Vagrant + libvirt provider; your user must be in the libvirt group (log out/in after adding).
sudo apt-get install -y vagrant libvirt-daemon-system qemu-kvm
vagrant plugin install vagrant-libvirt
sudo usermod -aG libvirt "$USER"

# retyc-cli (released binary or your own build), authenticated. `make vm-up` copies this exact
# binary into the VM, so whatever you test with in the VM is what you have on the host.
retyc auth status --json

# A long-lived offline token for non-interactive use (printed once; store it as a secret):
retyc auth login --offline

# The passphrase of your AGE key — never on the command line, read it into the environment:
read -rs RETYC_KEY_PASSPHRASE && export RETYC_KEY_PASSPHRASE
export RETYC_TOKEN=<offline token>
```

Then the VM:

```sh
make vm-up        # vagrant up (box download + k3s install on first run, a few minutes) + retyc copy
make vm-ssh       # a shell inside; kubectl works without sudo there
```

| Target | Does |
|--------|------|
| `make vm-up` | `vagrant up --provider=libvirt`, then `make vm-retyc` |
| `make vm-retyc` | Copies the host's `retyc` into the VM (`RETYC_BIN=…` to pick another) |
| `make vm-load` | `docker save` the driver image into k3s' containerd |
| `make vm-secret` | Creates the `retyc-csi-credentials` Secret from `RETYC_TOKEN`/`RETYC_KEY_PASSPHRASE` |
| `make vm-deploy` | `vagrant rsync` + applies `deploy/*.yaml` inside the VM |
| `make vm-ssh` | `vagrant ssh` |
| `make vm-destroy` | `vagrant destroy -f` (the box stays cached) |

The VM has 2 vCPUs, 4 GB RAM, NAT to the internet (it must reach `api.retyc.com`), `davfs2`
pre-configured like the container image (`/etc/davfs2/davfs2.conf`), and k3s with traefik
disabled. The repo is rsynced one-way to `/vagrant` (`vagrant rsync` to refresh it after edits).
Anything stateful you want to keep across `vagrant destroy` belongs on the host. The
`[fog][WARNING] Unrecognized arguments: libvirt_ip_command` line every vagrant command prints is
vagrant-libvirt 0.12.x noise, not an error.

## Stage 1 — WebDAV + davfs2 by hand

This is the riskiest assumption in the design (see the plan: eventually-consistent RWX), so test it
before anything else.

```sh
make vm-ssh
read -rs RETYC_KEY_PASSPHRASE && export RETYC_KEY_PASSPHRASE
export RETYC_TOKEN=<offline token>

# Pick or create a dataroom; the WebDAV path is its *title*, not its ID.
retyc --json dataroom create --title csi-manual-test

# 1. Serve
retyc webdav serve --addr 127.0.0.1 --port 8888 &

# 2. Mount it twice, as two independent clients (this is what two nodes look like)
sudo mkdir -p /mnt/a /mnt/b
sudo mount -t davfs -o dir_mode=0777,file_mode=0666 http://127.0.0.1:8888/dataroom/csi-manual-test /mnt/a
sudo mount -t davfs -o dir_mode=0777,file_mode=0666 http://127.0.0.1:8888/dataroom/csi-manual-test /mnt/b
```

Things to measure and write down:

```sh
# Write visibility latency between two mounts (expect a few seconds: davfs2 dir_refresh +
# retyc-cli's 30s listing cache, invalidated by mutations through the same server).
date > /mnt/a/hello.txt; time sh -c 'until cat /mnt/b/hello.txt 2>/dev/null; do sleep 0.5; done'

# Overwrite semantics: a PUT on an existing name creates a new *version* server-side.
echo v2 > /mnt/a/hello.txt; sleep 3; cat /mnt/b/hello.txt

# Concurrent writers to the same file (there is no cross-mount locking — last PUT wins).
(for i in $(seq 20); do echo a$i >> /mnt/a/race.txt; done) &
(for i in $(seq 20); do echo b$i >> /mnt/b/race.txt; done) & wait; sleep 3; wc -l /mnt/a/race.txt

# Non-root access (what a pod with runAsUser sees):
sudo -u nobody sh -c 'echo ok > /mnt/a/nobody.txt && cat /mnt/a/nobody.txt'

# mkdir / rename / delete
mkdir /mnt/a/dir && mv /mnt/a/hello.txt /mnt/a/dir/ && rm -r /mnt/a/dir

# Server restart while mounted (the node plugin restarts it on crash): kill and restart
# `retyc webdav serve`, then check the mounts recover on the next access.
```

Cleanup: `sudo umount /mnt/a /mnt/b; kill %1; retyc --json dataroom rm retyc://<id> -y`.

Also try the web app or another client (rclone, Finder) against the same dataroom while mounted:
changes made *outside* the local WebDAV server take up to ~1 min to appear (retyc-cli caches).

## Stage 2 — Unit tests and the real CLI

On the host:

```sh
make lint    # golangci-lint, same rules as retyc-cli (.golangci.yml)
make test    # go test -race ./...
```

`internal/retycclient` is tested against a fake `retyc` script that reproduces the real CLI's
stdout/stderr, including the spinner's ANSI escapes on stderr. When `retyc-cli` changes its JSON
shapes, re-capture them and update the fixtures:

```sh
retyc --json dataroom create --title fixture-check        # → {"id","title"}
retyc --json dataroom ls | head -20                        # → {"items":[...],"total","page","pages"}
retyc --json user quota                                    # → api.UserQuota
retyc --json dataroom rm retyc://<id> -y                   # → {"dataroom_id","uri","deleted_count"}
retyc --json dataroom rm retyc://00000000-0000-0000-0000-000000000000 -y   # → exit 1, stderr JSON 404
```

## Stage 3 — csi-sanity (optional, cheap once the VM exists)

[csi-sanity](https://github.com/kubernetes-csi/csi-test) drives the gRPC API the way kubelet and
the sidecars do, without a cluster. The Node service mounts for real, so it runs as root in the
VM. It creates and deletes real datarooms.

Build both binaries on the host (the VM has no Go toolchain) and ship them through the synced
folder:

```sh
make build
GOBIN=$PWD/dist go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@latest
vagrant rsync   # /vagrant is one-way host→guest; rsync__exclude drops dist/ and the binary, so:
vagrant ssh -c "sudo install -m 0755 /dev/stdin /usr/local/bin/retyc-k8s-csi" < retyc-k8s-csi
vagrant ssh -c "sudo install -m 0755 /dev/stdin /usr/local/bin/csi-sanity"    < dist/csi-sanity
```

In the VM (`make vm-ssh`, credentials exported as in stage 1):

```sh
# Controller and node in two processes, two sockets (both need RETYC_TOKEN / RETYC_KEY_PASSPHRASE):
sudo -E retyc-k8s-csi --mode=controller --endpoint=unix:///tmp/csi-ctrl.sock --retyc-bin=$(command -v retyc) &
sudo -E retyc-k8s-csi --mode=node       --endpoint=unix:///tmp/csi-node.sock --retyc-bin=$(command -v retyc) &

sudo -E csi-sanity \
  --csi.controllerendpoint=/tmp/csi-ctrl.sock \
  --csi.endpoint=/tmp/csi-node.sock \
  --csi.mountdir=/tmp/csi-mnt --csi.stagingdir=/tmp/csi-stage \
  --ginkgo.skip='(Snapshot|Expand|Block|ListVolumes|GetCapacity)'
```

Expected: everything under `Identity`, `Controller` (`CreateVolume`/`DeleteVolume`/
`ValidateVolumeCapabilities`) and `Node` passes; skipped groups are features the driver
deliberately doesn't advertise.

## Stage 4 — Single-node k3s

A privileged DaemonSet with `mountPropagation: Bidirectional` and FUSE needs a real Linux kernel
you control; that's the VM. Everything below runs from the host unless stated otherwise.

### 4.1 Build, load, deploy

k3s doesn't pull from your local Docker daemon; the image is shipped over SSH into its containerd
(the `:dev` tag gives `imagePullPolicy: IfNotPresent`, so no registry is involved).

```sh
make image          # optionally RETYC_VERSION=vX.Y.Z
make vm-load        # docker save | k3s ctr images import
make vm-secret      # retyc-csi-credentials from $RETYC_TOKEN / $RETYC_KEY_PASSPHRASE
make vm-deploy      # rsync + kubectl apply deploy/*.yaml in the VM

vagrant ssh -c "kubectl -n kube-system get pods -l 'app in (retyc-csi-controller, retyc-csi-node)' -w"
```

Both pods must be `Running` with all containers ready. If not (in the VM):

```sh
kubectl -n kube-system logs deploy/retyc-csi-controller -c retyc-csi-controller
kubectl -n kube-system logs ds/retyc-csi-node -c retyc-csi-node        # includes `retyc webdav serve` output
kubectl -n kube-system logs ds/retyc-csi-node -c node-driver-registrar
kubectl get csidriver csi.retyc.com                                    # attachRequired must be false
```

Iterating on the driver is `make image vm-load` then, in the VM,
`kubectl -n kube-system rollout restart deploy/retyc-csi-controller ds/retyc-csi-node`.

### 4.2 Provision and use an RWX volume

In the VM:

```sh
kubectl apply -f /vagrant/deploy/examples/rwx-test.yaml
kubectl get pvc retyc-rwx-test -w                 # Pending → Bound (a dataroom was created)
kubectl get pods retyc-writer retyc-reader -w      # both Running
kubectl logs -f retyc-reader                       # lines written by the writer show up every 5s
```

Cross-check from the host that the volume is a real dataroom:

```sh
retyc --json dataroom ls | grep -B1 -A3 '"pvc-'     # title == PV name
```

Then inspect the node side in the VM:

```sh
mount | grep davfs                            # one davfs2 mount per PV under /var/lib/kubelet/plugins/.../globalmount
mount | grep /var/lib/kubelet/pods             # one bind mount per pod
kubectl exec retyc-reader -- id
kubectl exec retyc-reader -- sh -c 'echo from-uid-1000 >> /data/log.txt'   # non-root write works
```

### 4.3 Failure modes worth exercising

In the VM:

```sh
# Node plugin restart: the FUSE daemons die with the container (documented limitation).
kubectl -n kube-system delete pod -l app=retyc-csi-node
kubectl exec retyc-writer -- ls /data               # expect "Transport endpoint is not connected"
kubectl delete pod retyc-writer retyc-reader && kubectl apply -f /vagrant/deploy/examples/rwx-test.yaml   # remount recovers

# Bad credentials: the node plugin's `retyc webdav serve` exits and is restarted every 5s;
# NodeStageVolume fails with Unavailable after 30s instead of hanging.
kubectl -n kube-system create secret generic retyc-csi-credentials --from-literal=RETYC_TOKEN=bad \
  --from-literal=RETYC_KEY_PASSPHRASE=bad --dry-run=client -o yaml | kubectl apply -f -
kubectl -n kube-system rollout restart ds/retyc-csi-node deploy/retyc-csi-controller
# ... then `make vm-secret` from the host and restart again to put the real ones back.
```

### 4.4 Teardown — and verify the dataroom is gone

```sh
vagrant ssh -c "kubectl delete -f /vagrant/deploy/examples/rwx-test.yaml"
vagrant ssh -c "kubectl get pv"               # released PV disappears (reclaimPolicy: Delete)
retyc --json dataroom ls | grep -c '"pvc-'    # 0 — DeleteVolume actually removed the dataroom
retyc --json user quota                       # count_dataroom back to where it started
```

`vagrant halt` to stop the VM and keep it, `make vm-destroy` to start fresh next time (the box
stays cached in `~/.vagrant.d/boxes`, so the rebuild is only the k3s install).

## What "good" looks like

- Stage 1: writes visible across mounts in single-digit seconds; non-root can write; no data
  corruption on sequential use; concurrent appends to one file lose lines (expected — no locking).
- Stage 4: PVC bound in < 30s, both pods running, reader sees writer's lines, dataroom appears
  and disappears in `retyc dataroom ls` in lockstep with the PVC.

## Known gaps this doesn't cover

- Multi-node clusters — cross-node visibility is only approximated by stage 1's two mounts
  against one server. Real nodes each run their own `retyc webdav serve`, so changes made on
  node A reach node B through the API only after A's cache invalidation *and* B's cache expiry
  (up to ~30s + `dir_refresh`). A second VM joining the k3s server as an agent would cover it;
  the `Vagrantfile` is single-machine on purpose for now.
- Load/perf: every read is a chunk download + AGE decrypt; nothing here measures throughput.
