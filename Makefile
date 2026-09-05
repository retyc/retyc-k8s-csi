MODULE       := github.com/retyc/retyc-k8s-csi
BINARY       := retyc-k8s-csi
IMAGE        ?= retyc/retyc-k8s-csi:dev
RETYC_VERSION ?= latest
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION     := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CREATED      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS      := -s -w -X $(MODULE)/internal/driver.DriverVersion=$(VERSION)

CHART        := charts/retyc-csi

.PHONY: build test vet lint clean image helm-lint helm-template helm-package \
	vm-up vm-retyc vm-load vm-secret vm-helm vm-restart vm-ssh vm-destroy

## Build the driver binary (both controller and node modes)
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) .

## Run tests with race detector
test:
	go test -race ./...

## Run go vet
vet:
	go vet ./...

## Run golangci-lint
lint:
	golangci-lint run ./...

## Remove built binary
clean:
	rm -f $(BINARY)

## Build the container image (controller + node share one image, see main.go --mode), with OCI
## provenance labels from git. Override the embedded retyc-cli version with e.g.
## `make image RETYC_VERSION=v0.3.0`, the image version with `VERSION=v1.0.0`.
image:
	docker build --pull \
	  --build-arg RETYC_VERSION=$(RETYC_VERSION) \
	  --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) --build-arg CREATED=$(CREATED) \
	  -t $(IMAGE) .

## Lint the Helm chart (values schema included) and render it in its three credential modes.
helm-lint:
	helm lint $(CHART)
	helm template x $(CHART) --set credentials.token=t --set credentials.keyPassphrase=p >/dev/null
	helm template x $(CHART) --set credentials.existingSecret=s >/dev/null
	helm template x $(CHART) >/dev/null

## Render the chart with default values to stdout.
helm-template:
	helm template retyc-csi $(CHART) -n kube-system

## Package the chart into dist/.
helm-package:
	helm package $(CHART) -d dist

## ---------------------------------------------------------------------------------------------
## Test VM: single-node k3s on Debian trixie under Vagrant + libvirt (Vagrantfile, doc/testing.md).
## Nothing in this Makefile talks to a cluster from the host: every vm-* target goes through
## `vagrant ssh -c`, a login shell in the VM where kubectl and helm work without sudo.

## Boot + provision the VM (first run: box download + k3s install, a few minutes), then ship the
## host's `retyc` CLI into it.
vm-up:
	vagrant up --provider=libvirt
	$(MAKE) vm-retyc

## Copy the host's `retyc` binary into the VM (stages 1 and 3 run it there). RETYC_BIN overrides.
RETYC_BIN ?= $(shell command -v retyc)
vm-retyc:
	@test -n "$(RETYC_BIN)" || { echo "retyc not found in PATH; set RETYC_BIN=/path/to/retyc"; exit 1; }
	vagrant ssh -c "sudo tee /usr/local/bin/retyc >/dev/null && sudo chmod 0755 /usr/local/bin/retyc" < $(RETYC_BIN)

## Import the locally built image straight into k3s' containerd (no registry involved).
vm-load:
	docker save $(IMAGE) | vagrant ssh -c "sudo k3s ctr images import -"

## Create/refresh the credentials Secret from RETYC_TOKEN / RETYC_KEY_PASSPHRASE in the host
## environment. Values travel over stdin, never on a command line.
vm-secret:
	@test -n "$$RETYC_TOKEN" && test -n "$$RETYC_KEY_PASSPHRASE" || { echo "export RETYC_TOKEN and RETYC_KEY_PASSPHRASE first (see doc/testing.md)"; exit 1; }
	@printf 'RETYC_TOKEN=%s\nRETYC_KEY_PASSPHRASE=%s\n' "$$RETYC_TOKEN" "$$RETYC_KEY_PASSPHRASE" | \
	  vagrant ssh -c "kubectl -n kube-system create secret generic retyc-csi-credentials --from-env-file=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -"

## Install or upgrade the chart in the VM against the Secret created by vm-secret (Helm is
## provisioned with the VM). Typical loop: make image vm-load vm-helm vm-restart
vm-helm:
	vagrant rsync
	vagrant ssh -c "helm upgrade --install retyc-csi /vagrant/$(CHART) -n kube-system --set image.tag=dev --set credentials.existingSecret=retyc-csi-credentials --wait --timeout 5m"

## Restart controller + node plugin so they pick up a freshly loaded :dev image (IfNotPresent +
## unchanged tag = no rollout on its own), and wait for both. Typical loop: make image vm-load vm-restart
vm-restart:
	vagrant ssh -c "kubectl -n kube-system rollout restart deploy/retyc-csi-controller ds/retyc-csi-node && kubectl -n kube-system rollout status deploy/retyc-csi-controller --timeout=180s && kubectl -n kube-system rollout status ds/retyc-csi-node --timeout=180s"

## Shell into the VM.
vm-ssh:
	vagrant ssh

## Destroy the VM (the downloaded box stays in ~/.vagrant.d/boxes).
vm-destroy:
	vagrant destroy -f
