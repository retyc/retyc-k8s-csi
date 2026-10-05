# -*- mode: ruby -*-
# vi: set ft=ruby :
#
# Single-node k3s test VM for retyc-k8s-csi (Debian 13 "trixie", libvirt/KVM). See doc/testing.md.
#
# Host requirements: vagrant + vagrant-libvirt (`vagrant plugin install vagrant-libvirt`), a
# running libvirtd with the qemu:///system URI, and your user in the `libvirt` group.
# Drive it through the `make vm-*` targets rather than calling vagrant directly.

Vagrant.configure("2") do |config|
  config.vm.box = "debian/trixie64"
  config.vm.hostname = "retyc-csi"

  # Traefik's entrypoints: web (the examples' ingresses) and jaeger (the Jaeger UI).
  config.vm.network "forwarded_port", guest: 80, host: 18080
  config.vm.network "forwarded_port", guest: 8081, host: 18081

  # rsync, not NFS/9p: host→guest one-way is all we need (the chart and the examples), and it
  # needs neither an NFS server nor sudo on the host. Re-run `vagrant rsync` after editing.
  config.vm.synced_folder ".", "/vagrant", type: "rsync",
    rsync__exclude: [".git/", ".vagrant/", "retyc-k8s-csi", "dist/"]

  config.vm.provider :libvirt do |lv|
    lv.driver = "kvm"
    lv.cpus = 2
    lv.memory = 4096
  end

  config.vm.provision "shell", name: "base", inline: <<~'SHELL'
    set -euo pipefail
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q
    apt-get install -y -q --no-install-recommends davfs2 curl ca-certificates rsync jq vim

    # Same davfs2 tuning the container image ships (see Dockerfile), so the manual stage-1 checks
    # measure exactly what the node plugin will see.
    cat > /etc/davfs2/davfs2.conf <<'EOF'
    ask_auth      0
    use_locks     0
    delay_upload  0
    dir_refresh   5
    file_refresh  1
    gui_optimize  0
    EOF

    # k3s, with its bundled Traefik serving the examples' ingresses; a 0644 kubeconfig lets the
    # vagrant user run kubectl without sudo (KUBECONFIG is exported for login shells, which is what
    # `vagrant ssh -c` uses).
    if ! command -v k3s >/dev/null 2>&1; then
      curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="--write-kubeconfig-mode 644" sh -
    fi
    echo 'export KUBECONFIG=/etc/rancher/k3s/k3s.yaml' > /etc/profile.d/k3s.sh

    until k3s kubectl wait --for=condition=Ready node --all --timeout=10s >/dev/null 2>&1; do sleep 2; done
    k3s kubectl get nodes

    # Helm, the way the driver is deployed (make vm-helm). Debian has no package; the official
    # installer verifies the release checksum before installing to /usr/local/bin.
    if ! command -v helm >/dev/null 2>&1; then
      curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-4 | bash
    fi
    helm version --short
  SHELL

  # Jaeger all-in-one (in-memory storage, the chart's default) collecting the retyc CLI's traces
  # (OTEL_* in examples/vagrant-values.yaml), its UI on http://localhost:18081. Traefik gets a
  # second entrypoint for it; web is made the default one, so host-less ingresses such as the
  # examples' stay on port 80 instead of also catching the Jaeger port.
  config.vm.provision "shell", name: "jaeger", inline: <<~'SHELL'
    set -euo pipefail
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

    cat > /var/lib/rancher/k3s/server/manifests/traefik-config.yaml <<'EOF'
    apiVersion: helm.cattle.io/v1
    kind: HelmChartConfig
    metadata:
      name: traefik
      namespace: kube-system
    spec:
      valuesContent: |-
        ports:
          web:
            asDefault: true
          jaeger:
            port: 8081
            expose:
              default: true
            exposedPort: 8081
            protocol: TCP
    EOF

    helm repo add jaegertracing https://jaegertracing.github.io/helm-charts >/dev/null
    helm repo update jaegertracing >/dev/null
    helm upgrade --install jaeger jaegertracing/jaeger --version 4.14.1 \
      -n jaeger --create-namespace --wait --timeout 5m -f - <<'EOF'
    jaeger:
      ingress:
        enabled: true
        annotations:
          traefik.ingress.kubernetes.io/router.entrypoints: jaeger
        hosts: [""]
    EOF
  SHELL
end
