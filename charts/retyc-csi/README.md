# retyc-csi

Helm chart for the [Retyc CSI driver](https://github.com/retyc/retyc-k8s-csi): `ReadWriteMany` Kubernetes volumes
backed by Retyc datarooms, with end-to-end post-quantum encryption.

## Install

```sh
helm install retyc-csi ./charts/retyc-csi \
  --namespace kube-system \
  --set credentials.token="$RETYC_TOKEN" \
  --set credentials.keyPassphrase="$RETYC_KEY_PASSPHRASE"
```

Or point the chart at a Secret you manage yourself (keys `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE`):

```sh
helm install retyc-csi ./charts/retyc-csi -n kube-system --set credentials.existingSecret=retyc-csi-credentials
```

Without either, the driver runs with no cluster-wide identity and only serves StorageClasses that carry a
`tenantSecretName` (per-namespace identities).

The chart installs the `CSIDriver`, RBAC, the controller `Deployment`, the node `DaemonSet` and three StorageClasses
(`retyc-rwx`, `retyc-rwx-retain`, `retyc-rwx-tenant`). Restarting the node plugin breaks the mounts on that node, so the
DaemonSet rolls one node at a time; plan upgrades like node drains.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.repository` | `retyc/retyc-k8s-csi` | Driver image |
| `image.tag` | `""` | Overrides the image tag; defaults to the chart's `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | Pull secrets for the driver image |
| `credentials.create` | `true` | Create the cluster-wide identity Secret from `token` and `keyPassphrase` |
| `credentials.existingSecret` | `""` | Existing Secret with `RETYC_TOKEN` and `RETYC_KEY_PASSPHRASE`; takes precedence |
| `credentials.token` | `""` | Offline token (`retyc auth login --offline`) |
| `credentials.keyPassphrase` | `""` | Passphrase of the account's AGE key |
| `storageClasses` | three classes | List of StorageClasses: `name`, `reclaimPolicy`, `volumeBindingMode`, `isDefault`, `annotations`, `tenantSecretName` |
| `rbac.create` | `true` | Create the provisioner ClusterRole/Binding (Secrets access added when a class uses `tenantSecretName`) |
| `serviceAccount.controller.*` / `serviceAccount.node.*` | create | `create`, `name`, `annotations` |
| `healthPort` | `9808` | `/healthz` and `/readyz` port of the driver containers |
| `logLevel` | `2` | klog verbosity |
| `probes.liveness` / `probes.readiness` | see values | Probe timings, shared by both components |
| `controller.replicas` | `1` | Add `--leader-election` to `controller.provisioner.extraArgs` for more |
| `controller.resources` | 64Mi / 512Mi | Driver container resources |
| `controller.provisioner.image.*` | `csi-provisioner:v6.3.0` | Sidecar image |
| `controller.provisioner.extraArgs` | `[]` | |
| `controller.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,priorityClassName}` | | Scheduling |
| `node.kubeletDir` | `/var/lib/kubelet` | Kubelet root on the nodes |
| `node.webdavPort` | `8888` | First loopback port of the `retyc webdav serve` servers |
| `node.stateDir` | `/var/lib/retyc-csi` | Per-identity state inside the container |
| `node.resources` | 64Mi / 512Mi | Keep the limit above ~350 MiB per identity (scrypt peak) |
| `node.registrar.image.*` | `csi-node-driver-registrar:v2.17.0` | Sidecar image |
| `node.updateStrategy` | `RollingUpdate`, `maxUnavailable: 1` | One node at a time |
| `node.{podAnnotations,podLabels,nodeSelector,tolerations,affinity,priorityClassName}` | tolerate all | Scheduling |

Per-tenant identities: a StorageClass entry with `tenantSecretName: retyc-credentials` resolves that Secret in the
claim's namespace for provisioning and mounting, through the standard `csi.storage.k8s.io/*-secret-*` parameters.
Use `reclaimPolicy: Retain` on such classes.
