IMG ?= ghcr.io/amayabdaniel/wavekube:latest
ENVTEST_K8S_VERSION = 1.30.0

.PHONY: all
all: test build

## Build
.PHONY: build
build:
	go build -o bin/manager cmd/main.go

.PHONY: run
run:
	go run cmd/main.go

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

## Test
.PHONY: test
test:
	go test ./... -coverprofile cover.out

.PHONY: test-unit
test-unit:
	go test ./internal/controller/... -v -run TestGNodeB

.PHONY: test-integration
test-integration:
	go test ./test/integration/... -v -tags=integration

.PHONY: test-e2e
test-e2e:
	go test ./test/e2e/... -v -tags=e2e -timeout 10m

## Install/Deploy
.PHONY: install
install:
	kubectl apply -f config/crd/bases/

.PHONY: uninstall
uninstall:
	kubectl delete -f config/crd/bases/

.PHONY: deploy
deploy:
	helm upgrade --install wavekube deploy/helm/wavekube/ -n wavekube-system --create-namespace

.PHONY: undeploy
undeploy:
	helm uninstall wavekube -n wavekube-system

## Dev tools
.PHONY: lint
lint:
	golangci-lint run

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: generate
# NOTE: api/v1alpha1/zz_generated.deepcopy.go is kept gofmt-canonical (ecf51c1);
# the `gofmt -w ./api` step below canonicalises whatever controller-gen emits so a
# generator that produces condensed single-line control flow can't reintroduce
# drift. Round-trip verified: after `make generate`, `gofmt -l ./api` is empty.
# crd:allowDangerousTypes=true is REQUIRED — the API intentionally uses float64
# (RANMetrics throughput/BLER, RANSecurityPolicy MaxCVSSScore) and the committed
# CRDs already carry them as `type: number`; controller-gen v0.16+ refuses floats
# without this flag and the CRD step halts.
# controller-gen version bind (worth knowing before treating a generate diff as
# real drift): v0.15.x won't COMPILE under Go 1.26 (its x/tools dep is too old),
# while v0.22 builds but emits a leaner deepcopy and empty-group CRD filenames
# (`_gnodebs.yaml`) — so a `make generate` diff may be generator-version drift,
# not a source change. Pin a known-good controller-gen before adopting its output.
generate:
	controller-gen object paths="./api/..."
	gofmt -w ./api
	controller-gen crd:allowDangerousTypes=true paths="./api/..." output:crd:artifacts:config=config/crd/bases

.PHONY: kind-setup
kind-setup:
	kind create cluster --name wavekube-dev
	kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml || true

.PHONY: kind-teardown
kind-teardown:
	kind delete cluster --name wavekube-dev
