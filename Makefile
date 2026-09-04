MODULE       := github.com/retyc/retyc-k8s-csi
BINARY       := retyc-k8s-csi
IMAGE        ?= retyc/retyc-k8s-csi:dev
RETYC_VERSION ?= latest
MANIFESTS    := deploy/csidriver.yaml deploy/rbac.yaml deploy/csi-controller.yaml deploy/csi-node-daemonset.yaml deploy/storageclass.yaml

.PHONY: build test vet lint clean image deploy vm-up vm-retyc vm-load vm-secret vm-deploy vm-ssh vm-destroy

## Build the driver binary (both controller and node modes)
build:
	go build -o $(BINARY) .

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

## Build the container image (controller + node share one image, see main.go --mode).
## Override the embedded retyc-cli version with e.g. `make image RETYC_VERSION=v0.3.0`.
image:
	docker build --pull --build-arg RETYC_VERSION=$(RETYC_VERSION) -t $(IMAGE) .

## Apply every manifest under deploy/ (StorageClass, RBAC, Controller, DaemonSet) —
## deploy/secret.yaml.example is intentionally excluded: copy it, fill in real
## credentials, and apply it yourself.
deploy:
	kubectl apply $(addprefix -f ,$(MANIFESTS))

## ---------------------------------------------------------------------------------------------
## Test VM: single-node k3s on Debian trixie under Vagrant + libvirt (Vagrantfile, doc/testing.md).
## Every vm-* target runs from the host; `vagrant ssh -c` opens a login shell, so kubectl works
## without sudo inside (KUBECONFIG is set by /etc/profile.d/k3s.sh).

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

## Sync deploy/ into the VM and apply the driver manifests there (same set as `make deploy`).
vm-deploy:
	vagrant rsync
	vagrant ssh -c "kubectl apply $(addprefix -f /vagrant/,$(MANIFESTS))"

## Shell into the VM.
vm-ssh:
	vagrant ssh

## Destroy the VM (the downloaded box stays in ~/.vagrant.d/boxes).
vm-destroy:
	vagrant destroy -f
