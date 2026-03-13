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
generate:
	controller-gen object paths="./api/..."
	controller-gen crd paths="./api/..." output:crd:artifacts:config=config/crd/bases

.PHONY: kind-setup
kind-setup:
	kind create cluster --name wavekube-dev
	kubectl apply -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.15.0/deployments/static/nvidia-device-plugin.yml || true

.PHONY: kind-teardown
kind-teardown:
	kind delete cluster --name wavekube-dev
