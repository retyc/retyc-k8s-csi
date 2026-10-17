MODULE       := github.com/retyc/retycfs-k8s-csi
BINARY       := retycfs-k8s-csi
IMAGE        ?= ghcr.io/retyc/retycfs-k8s-csi:dev
RETYC_CLI_IMAGE ?= ghcr.io/retyc/retyc-cli
CLI_IMAGE    ?= retyc-cli:retycfs-v1.5.0-beta2
CLI_SRC      ?= ../retyc_cli
CLI_VERSION  ?= v1.5.0-beta2
VERSION      ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REVISION     := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
CREATED      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
EPOCH        := $(shell date -u +%s)
LDFLAGS      := -s -w -X $(MODULE)/internal/driver.DriverVersion=$(VERSION)

CHART        := charts/retycfs-csi

.PHONY: build test vet lint vulncheck clean image cli-image helm-lint helm-template helm-package \
	vm-up vm-retyc vm-load vm-load-cli vm-secret vm-helm vm-restart vm-ssh vm-destroy

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

vulncheck:
	govulncheck ./...

clean:
	rm -f $(BINARY)

## Build the container image (controller + node share one image, see main.go --mode), with OCI
## provenance labels from git. The embedded retyc-cli version is pinned in the Dockerfile;
## `make image RETYC_CLI_VERSION=commit-xxxxxxx` overrides it for a one-off build, `RETYC_CLI_IMAGE`
## the CLI image repository, `VERSION=v1.0.0` the image version.
image:
	docker build \
	  $(if $(RETYC_CLI_VERSION),--no-cache --build-arg RETYC_CLI_VERSION=$(RETYC_CLI_VERSION) --build-arg SOURCE_DATE_EPOCH=0) \
	  --build-arg RETYC_CLI_IMAGE=$(RETYC_CLI_IMAGE) \
	  --build-arg VERSION=$(VERSION) --build-arg REVISION=$(REVISION) --build-arg CREATED=$(CREATED) \
	  -t $(IMAGE) .

## Build the development retyc-cli image of the mount pods (CLI_IMAGE, as examples/vagrant-values.yaml
## expects) from the tag CLI_VERSION of a retyc-cli checkout (CLI_SRC). The API refuses a CLI without a
## version (426), hence VERSION; retyc-cli's Dockerfile builds with `-tags prod`, which ignores
## RETYC_INSECURE: the build uses a copy without it, in an export of the tag (the checkout is untouched).
cli-image:
	@src=$$(mktemp -d) && git -C $(CLI_SRC) archive $(CLI_VERSION) | tar -x -C $$src && \
	sed '/-tags prod/d' $$src/Dockerfile > $$src/Dockerfile.dev && \
	docker build -f $$src/Dockerfile.dev --build-arg VERSION=$(CLI_VERSION) -t $(CLI_IMAGE) $$src; \
	rc=$$?; rm -rf $$src; exit $$rc

## Lint the Helm chart (values schema included), check that it runs the Dockerfile's retyc-cli, and
## render it in its three credential modes.
helm-lint:
	helm dependency build $(CHART)
	helm lint $(CHART)
	.github/scripts/check-cli-version.sh $(CHART)
	helm template x $(CHART) --set credentials.token=t --set credentials.keyPassphrase=p >/dev/null
	helm template x $(CHART) --set credentials.existingSecret=s >/dev/null
	helm template x $(CHART) >/dev/null

## Render the chart with default values to stdout.
helm-template:
	helm template retycfs-csi $(CHART) -n kube-system

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

## Import the retyc-cli image (used by the mount pods) into k3s' containerd. CLI_IMAGE overrides it.
vm-load-cli:
	docker save $(CLI_IMAGE) | vagrant ssh -c "sudo k3s ctr images import -"

## Create/refresh the credentials Secret from RETYC_TOKEN / RETYC_KEY_PASSPHRASE in the host
## environment. Values travel over stdin, never on a command line.
vm-secret:
	@test -n "$$RETYC_TOKEN" && test -n "$$RETYC_KEY_PASSPHRASE" || { echo "export RETYC_TOKEN and RETYC_KEY_PASSPHRASE first (see doc/testing.md)"; exit 1; }
	@printf 'RETYC_TOKEN=%s\nRETYC_KEY_PASSPHRASE=%s\n' "$$RETYC_TOKEN" "$$RETYC_KEY_PASSPHRASE" | \
	  vagrant ssh -c "kubectl -n kube-system create secret generic retycfs-csi-credentials --from-env-file=/dev/stdin --dry-run=client -o yaml | kubectl apply -f -"

## Install or upgrade the chart in the VM with examples/vagrant-values.yaml: the :dev image, the
## Secret created by vm-secret, traces to the provisioned Jaeger (Helm is provisioned with the VM).
## Typical loop: make image vm-load vm-helm vm-restart
VM_VALUES ?= examples/vagrant-values.yaml
vm-helm:
	vagrant rsync
	vagrant ssh -c "helm upgrade --install retycfs-csi /vagrant/$(CHART) -n kube-system -f /vagrant/$(VM_VALUES) --wait --timeout 5m"

## Restart controller + node plugin so they pick up a freshly loaded :dev image (IfNotPresent +
## unchanged tag = no rollout on its own), and wait for both. Typical loop: make image vm-load vm-restart
vm-restart:
	vagrant ssh -c "kubectl -n kube-system rollout restart deploy/retycfs-csi-controller ds/retycfs-csi-node && kubectl -n kube-system rollout status deploy/retycfs-csi-controller --timeout=180s && kubectl -n kube-system rollout status ds/retycfs-csi-node --timeout=180s"

## Shell into the VM.
vm-ssh:
	vagrant ssh

## Destroy the VM (the downloaded box stays in ~/.vagrant.d/boxes).
vm-destroy:
	vagrant destroy -f
