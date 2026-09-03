# Testing retyc-k8s-csi

Four stages, cheapest first. Each one validates an assumption the next one depends on, so don't
skip straight to the cluster.

| Stage | What it proves | Needs |
|-------|----------------|-------|
| 1. Manual WebDAV + davfs2 | The storage path works at all, and how consistent it is | Linux with `davfs2`, root, a Retyc account |
| 2. Unit tests + real CLI | The `retyc` exec wrapper decodes what the real CLI prints | Go, `retyc` binary |
| 3. csi-sanity (optional) | gRPC conformance of the Controller/Node services | Go, root, a Retyc account |
| 4. Single-node k3s in QEMU | The whole thing, including kubelet mount propagation and FUSE | QEMU/KVM, ~4 GB RAM |

Everything mutating talks to a **real Retyc account**: use a test account, and check
`retyc --json user quota` before/after — stages 3 and 4 create and delete datarooms.

## Prerequisites (all stages)

```sh
# retyc-cli built from ~/dev/RETYC/retyc-cli (or the released binary), authenticated:
retyc auth status --json

# A long-lived offline token for non-interactive use (printed once; store it as a secret):
retyc auth login --offline

# The passphrase of your AGE key — never on the command line, read it into the environment:
read -rs RETYC_KEY_PASSPHRASE && export RETYC_KEY_PASSPHRASE
export RETYC_TOKEN=<offline token>
```

## Stage 1 — WebDAV + davfs2 by hand

This is the riskiest assumption in the design (see the plan: eventually-consistent RWX), so test it
before anything else. Run it on a real Linux box or in the QEMU VM from stage 4 — **not** inside a
sandboxed shell: `mount.davfs` needs `/dev/fuse` and `CAP_SYS_ADMIN`, and dies with SIGABRT
without them.

```sh
sudo apt-get install -y davfs2

# Same tuning the Docker image ships (doc: Dockerfile). Without ask_auth=0 mount.davfs prompts
# for a username; without delay_upload=0 a closed file waits 10s before being uploaded.
sudo tee -a /etc/davfs2/davfs2.conf <<'EOF'
ask_auth      0
use_locks     0
delay_upload  0
dir_refresh   5
EOF

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

## Stage 3 — csi-sanity (optional, but cheap once stage 4's VM exists)

[csi-sanity](https://github.com/kubernetes-csi/csi-test) drives the gRPC API the way kubelet and
the sidecars do, without a cluster. The Node service mounts for real, so run it as root on a box
with `davfs2` (the stage 4 VM is ideal). It creates and deletes real datarooms.

```sh
go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@latest
make build

# Controller and node in two processes, two sockets (both need RETYC_TOKEN / RETYC_KEY_PASSPHRASE):
sudo -E ./retyc-k8s-csi --mode=controller --endpoint=unix:///tmp/csi-ctrl.sock --retyc-bin=$(command -v retyc) &
sudo -E ./retyc-k8s-csi --mode=node       --endpoint=unix:///tmp/csi-node.sock --retyc-bin=$(command -v retyc) &

sudo -E csi-sanity \
  --csi.controllerendpoint=/tmp/csi-ctrl.sock \
  --csi.endpoint=/tmp/csi-node.sock \
  --csi.mountdir=/tmp/csi-mnt --csi.stagingdir=/tmp/csi-stage \
  --ginkgo.skip='(Snapshot|Expand|Block|ListVolumes|GetCapacity)'
```

Expected: everything under `Identity`, `Controller` (`CreateVolume`/`DeleteVolume`/
`ValidateVolumeCapabilities`) and `Node` passes; skipped groups are features the driver
deliberately doesn't advertise.

## Stage 4 — Single-node k3s in QEMU

A privileged DaemonSet with `mountPropagation: Bidirectional` and FUSE needs a real Linux kernel
you control — a Debian cloud image under QEMU/KVM is the least-effort way to get one.

### 4.1 Build the VM

```sh
# Debian/Ubuntu host: qemu-img alone isn't enough, the system emulator is a separate package.
sudo apt-get install -y qemu-system-x86 qemu-utils cloud-image-utils genisoimage

mkdir -p ~/vm/retyc-csi && cd ~/vm/retyc-csi
curl -LO https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-amd64.qcow2
qemu-img create -f qcow2 -b debian-13-genericcloud-amd64.qcow2 -F qcow2 disk.qcow2 20G

# cloud-init seed: your SSH key + k3s (traefik disabled, not needed)
cat > user-data <<EOF
#cloud-config
users:
  - name: debian
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - $(cat ~/.ssh/id_ed25519.pub)
package_update: true
packages: [curl, davfs2]
runcmd:
  - curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--disable traefik" sh -
EOF
printf 'instance-id: retyc-csi\nlocal-hostname: retyc-csi\n' > meta-data
genisoimage -output seed.iso -volid cidata -joliet -rock user-data meta-data   # or: cloud-localds seed.iso user-data meta-data

qemu-system-x86_64 -enable-kvm -m 4G -smp 2 -cpu host \
  -drive file=disk.qcow2,if=virtio \
  -drive file=seed.iso,if=virtio,format=raw \
  -nic user,model=virtio,hostfwd=tcp::2222-:22 \
  -nographic
```

User-mode networking gives the VM NAT to the internet (it must reach `api.retyc.com`), and
`hostfwd` exposes SSH on `localhost:2222`. First boot takes a couple of minutes (packages + k3s).

```sh
ssh -p 2222 debian@localhost 'sudo k3s kubectl get nodes'   # STATUS Ready
```

### 4.2 Load the image and deploy

k3s doesn't pull from your local Docker daemon; ship the image over SSH into its containerd
(the `:dev` tag gives `imagePullPolicy: IfNotPresent`, so no registry is involved):

```sh
make image                                   # optionally RETYC_VERSION=vX.Y.Z
docker save retyc/retyc-k8s-csi:dev | ssh -p 2222 debian@localhost 'sudo k3s ctr images import -'

scp -P 2222 -r deploy debian@localhost:
ssh -p 2222 debian@localhost
```

In the VM:

```sh
alias k='sudo k3s kubectl'

# The shared identity, built from your host's `retyc auth login --offline` output:
k -n kube-system create secret generic retyc-csi-credentials \
  --from-literal=RETYC_TOKEN="$RETYC_TOKEN" \
  --from-literal=RETYC_KEY_PASSPHRASE="$RETYC_KEY_PASSPHRASE"

k apply -f deploy/csidriver.yaml -f deploy/rbac.yaml -f deploy/csi-controller.yaml \
        -f deploy/csi-node-daemonset.yaml -f deploy/storageclass.yaml

k -n kube-system get pods -l 'app in (retyc-csi-controller, retyc-csi-node)' -w
```

Both pods must be `Running` with all containers ready. If not:

```sh
k -n kube-system logs deploy/retyc-csi-controller -c retyc-csi-controller
k -n kube-system logs ds/retyc-csi-node -c retyc-csi-node        # includes `retyc webdav serve` output
k -n kube-system logs ds/retyc-csi-node -c node-driver-registrar
k get csidriver csi.retyc.com                                    # attachRequired must be false
```

### 4.3 Provision and use an RWX volume

```sh
k apply -f deploy/examples/rwx-test.yaml
k get pvc retyc-rwx-test -w                 # Pending → Bound (a dataroom was created)
k get pods retyc-writer retyc-reader -w      # both Running
k logs -f retyc-reader                       # lines written by the writer show up every 5s
```

Cross-check from the host that the volume is a real dataroom:

```sh
retyc --json dataroom ls | grep -B1 -A3 '"pvc-'     # title == PV name
```

Then inspect the node side inside the VM:

```sh
mount | grep davfs                            # one davfs2 mount per PV under /var/lib/kubelet/plugins/.../globalmount
mount | grep /var/lib/kubelet/pods             # one bind mount per pod
k exec retyc-reader -- id; k exec retyc-reader -- sh -c 'echo from-uid-1000 >> /data/log.txt'   # non-root write works
```

### 4.4 Failure modes worth exercising

```sh
# Node plugin restart: the FUSE daemons die with the container (documented limitation).
k -n kube-system delete pod -l app=retyc-csi-node
k exec retyc-writer -- ls /data               # expect "Transport endpoint is not connected"
k delete pod retyc-writer retyc-reader && k apply -f deploy/examples/rwx-test.yaml   # remount recovers

# Bad credentials: the node plugin's `retyc webdav serve` exits and is restarted every 5s;
# NodeStageVolume fails with Unavailable after 30s instead of hanging.
k -n kube-system create secret generic retyc-csi-credentials --from-literal=RETYC_TOKEN=bad \
  --from-literal=RETYC_KEY_PASSPHRASE=bad --dry-run=client -o yaml | k apply -f -
k -n kube-system rollout restart ds/retyc-csi-node deploy/retyc-csi-controller
```

### 4.5 Teardown — and verify the dataroom is gone

```sh
k delete -f deploy/examples/rwx-test.yaml
k get pv                                      # released PV disappears (reclaimPolicy: Delete)
retyc --json dataroom ls | grep -c '"pvc-'    # 0 — DeleteVolume actually removed the dataroom
retyc --json user quota                       # count_dataroom back to where it started
```

Stop the VM with `sudo poweroff`; delete `~/vm/retyc-csi/disk.qcow2` to start fresh (the base
image stays).

## What "good" looks like

- Stage 1: writes visible across mounts in single-digit seconds; non-root can write; no data
  corruption on sequential use; concurrent appends to one file lose lines (expected — no locking).
- Stage 4: PVC bound in < 30s, both pods running, reader sees writer's lines, dataroom appears
  and disappears in `retyc dataroom ls` in lockstep with the PVC.

## Known gaps this doesn't cover

- Multi-node clusters (two VMs) — cross-node visibility is only approximated by stage 1's two
  mounts against one server. Real nodes each run their own `retyc webdav serve`, so changes made
  on node A reach node B through the API only after A's cache invalidation *and* B's cache expiry
  (up to ~30s + `dir_refresh`).
- Load/perf: every read is a chunk download + AGE decrypt; nothing here measures throughput.
