MODULE       := github.com/retyc/retyc-k8s-csi
BINARY       := retyc-k8s-csi
IMAGE        ?= retyc/retyc-k8s-csi:dev
RETYC_VERSION ?= latest

.PHONY: build test vet lint image deploy

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
	docker build --build-arg RETYC_VERSION=$(RETYC_VERSION) -t $(IMAGE) .

## Apply every manifest under deploy/ (StorageClass, RBAC, Controller, DaemonSet) —
## deploy/secret.yaml.example is intentionally excluded: copy it, fill in real
## credentials, and apply it yourself.
deploy:
	kubectl apply -f deploy/csidriver.yaml -f deploy/rbac.yaml -f deploy/csi-controller.yaml -f deploy/csi-node-daemonset.yaml -f deploy/storageclass.yaml
