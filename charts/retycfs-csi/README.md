# retycfs-csi

Helm chart for the [RetycFS CSI driver](https://github.com/retyc/retycfs-k8s-csi): `ReadWriteMany` Kubernetes volumes
backed by Retyc datarooms, with end-to-end post-quantum encryption.

## Install

From the Helm repository:

```sh
helm repo add retyc https://retyc.github.io/retycfs-k8s-csi
helm install retycfs-csi retyc/retycfs-csi \
  --namespace kube-system \
  --set credentials.token="$RETYC_TOKEN" \
  --set credentials.keyPassphrase="$RETYC_KEY_PASSPHRASE"
```

From the OCI registry (`--version` selects a chart version; omit it for the latest):

```sh
helm install retycfs-csi oci://ghcr.io/retyc/charts/retycfs-csi --version <chart version> -n kube-system ...
```

Or point the chart at a Secret you manage yourself (keys `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE`):

```sh
helm install retycfs-csi retyc/retycfs-csi -n kube-system --set credentials.existingSecret=retycfs-csi-credentials
```

Without either, the driver runs with no cluster-wide identity: the StorageClasses without a `tenantSecretName` are
not installed, and only per-namespace identities are served.

The chart installs the `CSIDriver`, RBAC, the controller `Deployment`, the node `DaemonSet`, the mount pods'
configuration, a `ValidatingAdmissionPolicy` on the claims' `csi.retycfs.com/*` annotations and up to three
StorageClasses (`retycfs-rwx`, `retycfs-rwx-retain`, `retycfs-rwx-tenant`). Each staged volume runs in a privileged mount pod
(`retyc fs mount`) in the release namespace, which must allow it (Pod Security `privileged`). Restarting the node plugin
cuts no mount. Every claim needs the `csi.retycfs.com/uid` annotation: the files belong to that UID, and only pods running
as it can write. See [PVC annotations](../../doc/configuration.md#pvc-annotations) for the others.

**StatefulSets**: put `csi.retycfs.com/uid` in the `volumeClaimTemplates` (`metadata.annotations`). Without it, the
admission policy refuses each claim the StatefulSet controller creates, and the controller retries in a loop: the
StatefulSet's pods stay `Pending` and its events repeat the refusal.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `ghcr.io/retyc/retycfs-k8s-csi` | Driver image (Docker Hub mirror: `retyc/retycfs-k8s-csi`) |
| `image.tag` | `""` | Overrides the image tag; defaults to the chart's `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | Pull secrets for the driver image, both components (only for a private mirror) |
| `credentials.create` | `true` | Create the cluster-wide identity Secret from `token` and `keyPassphrase` (when `token` is set) |
| `credentials.existingSecret` | `""` | Existing Secret with `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE`; takes precedence |
| `credentials.token` | `""` | Offline token (`retyc auth login --offline`) |
| `credentials.keyPassphrase` | `""` | Passphrase of the account's AGE key |
| `storageClasses` | three classes | `name`, `reclaimPolicy`, `volumeBindingMode`, `isDefault`, `annotations`, `tenantSecretName`. All allow expansion; a class without `tenantSecretName` is only installed with a cluster-wide identity |
| `rbac.create` | `true` | Create the ClusterRoles/Roles and bindings |
| `serviceAccount.controller.*` / `serviceAccount.node.*` | create | `create`, `name`, `annotations` |
| `extraObjects` | `[]` | Extra manifests rendered as templates (see values.yaml for NetworkPolicy examples) |
| `healthPort` | `9808` | `/healthz`, `/readyz` and `/metrics` port of the driver containers |
| `metrics.podMonitor.enabled` | `false` | Create two Prometheus Operator `PodMonitor`s: both driver components, and the mount pods (port `metrics`, 9567) |
| `metrics.podMonitor.namespace` / `labels` | `""` / `{}` | Where to create them, and labels your Prometheus selects on |
| `metrics.podMonitor.interval` / `scrapeTimeout` / `relabelings` / `metricRelabelings` | Prometheus defaults | Scrape settings, shared by both |
| `logLevel` | `2` | klog verbosity |
| `probes.liveness` / `probes.readiness` | see values | Probe timings, shared by both components |
| `controller.replicas` | `1` | Above 1, `--leader-election` is added to csi-provisioner and csi-resizer (with a Role on leases) |
| `controller.resources` | requests `cpu: 10m`, `memory: 64Mi`; limits `memory: 512Mi` | Driver container resources (it runs the `retyc` CLI for each provisioning call) |
| `controller.provisioner.image.*` / `controller.resizer.image.*` | `csi-provisioner:v6.3.0` / `csi-resizer:v2.3.0` | Sidecar images |
| `controller.provisioner.extraArgs` / `controller.resizer.extraArgs` | `[]` | Extra sidecar arguments |
| `controller.extraEnv` | `[]` | Extra environment for the driver container (e.g. `HTTPS_PROXY`, `NO_PROXY`) |
| `controller.extraVolumes` / `controller.extraVolumeMounts` | `[]` | Extra volumes for the driver container (e.g. a private CA bundle) |
| `controller.hostAliases` | `[]` | Extra `/etc/hosts` entries |
| `controller.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,topologySpreadConstraints,priorityClassName}` | | Scheduling (anti-affinity example in values.yaml) |
| `node.kubeletDir` | `/var/lib/kubelet` | Kubelet root on the nodes |
| `node.resources` | requests `cpu: 10m`, `memory: 32Mi`; limits `memory: 128Mi` | The node plugin runs no retyc process |
| `node.registrar.image.*` | `csi-node-driver-registrar:v2.17.0` | Sidecar image |
| `node.extraArgs` / `node.extraEnv` | `[]` | Extra arguments and environment for the driver container |
| `node.extraVolumes` / `node.extraVolumeMounts` | `[]` | Extra volumes for the driver container |
| `node.updateStrategy` | `RollingUpdate`, `maxUnavailable: 1` | |
| `node.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,priorityClassName}` | tolerate all | Scheduling |
| `mount.image.*` | `ghcr.io/retyc/retyc-cli:v1.5.0-beta2` | Mount pods' image; must match the Dockerfile's `RETYC_CLI_VERSION` (checked in CI) |
| `mount.imagePullSecrets` | `[]` | Pull secrets of the retyc-cli image |
| `mount.extraEnv` / `mount.hostAliases` | `[]` | Extra environment (after the driver's) and `/etc/hosts` entries of the mount pods |
| `mount.hostPID` | `false` | Share the host PID namespace (diagnosis only) |
| `mount.resources` | 50m, 256Mi / 512Mi | Default resources, overridden per claim by the `csi.retycfs.com/mount-*` annotations |
| `mount.drainTimeout` | `5m` | Time a stopping mount pod gets to send its pending writes |
| `mount.priorityClassName` | `""` | Empty means `system-node-critical` |
| `mount.spoolDir` | `/var/lib/retycfs-csi/spool` | Host directory of the spools, one per volume |
| `cache.store` | `memory` | Listing cache of the mount pods: `memory` (one per pod) or `redis` (shared) |
| `cache.highConfidentiality` | `false` | Keep only ciphertexts in Redis; set it when several tenants share the Redis |
| `cache.redis.subchart.enabled` | `false` | Deploy the CloudPirates Redis chart (standalone, no persistence, `allkeys-lru`; values under `redis`) |
| `cache.redis.subchart.password` | `""` | Redis password; empty generates one and keeps it across upgrades (not in GitOps: see below) |
| `cache.redis.subchart.existingSecret` | `""` | Your own Secret instead, keys `redis-password` and `RETYC_CACHE_REDIS_URL`; set `redis.auth.existingSecret` to the same name |
| `cache.redis.external.url` | `""` | URL of your own Redis (7.4+, standalone), stored in a Secret by the chart |
| `cache.redis.external.existingSecret` / `existingSecretKey` | `""` / `RETYC_CACHE_REDIS_URL` | Existing Secret holding the URL instead |
| `admissionPolicy.enabled` | `true` | `ValidatingAdmissionPolicy` on the annotations of the claims of the chart's StorageClasses |
| `admissionPolicy.failurePolicy` | `Fail` | `Fail` or `Ignore` when the policy cannot be evaluated; only the claims of the chart's classes are matched |
| `redis.*` | see values | Values of the Redis subchart (`maxmemory`, resources...) |

The Redis URL never goes into the mount pods' ConfigMap: the node plugin reads it from a Secret (`RETYC_CACHE_REDIS_URL`)
and copies it into each mount pod's own Secret. The admission policy only knows the classes this release installs: a
claim given one of them later as the cluster default, or a static PV's claim on another class, is not checked (the
driver still refuses an invalid claim at provisioning or at mount).

**GitOps (Argo CD, `helm template`)**: set `cache.redis.subchart.password`, or use `cache.redis.subchart.existingSecret`.
The generated password relies on Helm's `lookup`, which returns nothing outside `helm install`/`upgrade`: each render
would generate a new one, while the Redis StatefulSet keeps the former one.

## Releasing a chart version

The chart is published by the `Chart` workflow when `Chart.yaml` changes on `master`: bump `version` (the chart)
and, when it should ship a new driver, `appVersion` to an image tag that exists (a `v*` release of this repository).
chart-releaser then creates the `retycfs-csi-<version>` GitHub release with the package, refreshes `index.yaml` on
the `gh-pages` branch, and the same package is pushed to `oci://ghcr.io/retyc/charts`.

Per-tenant identities: a StorageClass entry with `tenantSecretName: retycfs-credentials` resolves that Secret in the
claim's namespace for provisioning and mounting, through the standard `csi.storage.k8s.io/*-secret-*` parameters.
Use `reclaimPolicy: Retain` on such classes.
