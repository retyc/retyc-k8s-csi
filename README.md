<p align="center"><img width="200" src=".media/Retyc_Logo_Blue.png" alt="Retyc logo" /></p>

<p align="center">
  <a href="https://github.com/retyc/retyc-k8s-csi/actions/workflows/main.yml"><img src="https://github.com/retyc/retyc-k8s-csi/actions/workflows/main.yml/badge.svg" alt="CI" /></a>
  <a href="https://kubernetes-csi.github.io/docs/"><img src="https://img.shields.io/badge/CSI-1.11-326ce5.svg" alt="CSI 1.11" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/go-1.26-00ADD8.svg" alt="Go 1.26" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License: MIT" /></a>
</p>

# Retyc CSI Driver

**Retyc CSI driver** is a CSI driver that uses your [Retyc](https://retyc.com) account to support dynamic
provisioning of `ReadWriteMany` Kubernetes Persistent Volumes via Persistent Volume Claims. Each Persistent Volume is
a Retyc dataroom, named after the volume, encrypted end to end on the node before anything reaches the Retyc servers.

## How to deploy Retyc CSI driver to your cluster

You need a Retyc account and the [`retyc` CLI](https://github.com/retyc/retyc-cli). The driver uses two credentials:

- a token, printed once by `retyc auth login --offline`;
- the passphrase of your Retyc key, the one the CLI asks you for.

Your nodes must run Kubernetes 1.34 or later, allow privileged pods, have `/dev/fuse` and reach `api.retyc.com`.

### With Helm

Follow the instructions from the helm chart [README](charts/retyc-csi/README.md).

The tl;dr is

```console
$ helm repo add retyc https://retyc.github.io/retyc-k8s-csi/
$ helm install retyc-csi retyc/retyc-csi --namespace kube-system \
    --set credentials.token=<token> \
    --set credentials.keyPassphrase=<key passphrase>
```

Both pods must be `2/2 Running`; a `1/2` means the credentials are wrong (see
[doc/troubleshooting.md](doc/troubleshooting.md)):

```console
$ kubectl -n kube-system get pods -l app.kubernetes.io/instance=retyc-csi
```

### Test your environment

Deploy the test resources, a claim with a writer pod and a reader pod:

```console
$ kubectl create -f https://raw.githubusercontent.com/retyc/retyc-k8s-csi/master/examples/rwx-test.yaml
$ kubectl logs -f retyc-reader
```

The reader prints the lines the writer appends. Check `retyc dataroom ls`, or the Retyc web app: the dataroom is
there, named after the volume.

Delete the test resources:

```console
$ kubectl delete -f https://raw.githubusercontent.com/retyc/retyc-k8s-csi/master/examples/rwx-test.yaml
```

The dataroom is gone as well.

### Deploying your own PersistentVolumeClaims

Use `accessModes: [ReadWriteMany]` and one of the storage classes:

- `retyc-rwx`: deleting the claim deletes the dataroom;
- `retyc-rwx-retain`: deleting the claim keeps the dataroom;
- `retyc-rwx-tenant`: same, with the credentials of a `retyc-credentials` Secret in the claim's namespace instead of
  the cluster-wide ones (see [doc/configuration.md](doc/configuration.md#per-tenant-identities)).

Every pod mounting the claim, on any node, shares the same dataroom. It behaves like a network drive: writes made on
one node show up on the others a few seconds later, and concurrent writes to one file are not safe. Good for
documents, configuration and artefacts; not for databases.

## Documentation

- [charts/retyc-csi/README.md](charts/retyc-csi/README.md): chart values
- [doc/configuration.md](doc/configuration.md): per-namespace credentials, adopting an existing dataroom, flags
- [doc/troubleshooting.md](doc/troubleshooting.md)
- [doc/architecture.md](doc/architecture.md): how it works, consistency, security, limitations
- [doc/testing.md](doc/testing.md): development environment and tests

## Development

```console
$ make build test lint     # driver
$ make helm-lint           # chart
$ make image               # container image
$ make vm-up               # single-node k3s in a Vagrant VM, then: make image vm-load vm-secret vm-helm
```

## License

[MIT](LICENSE) - © Retyc / TripleStack SAS
