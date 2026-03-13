# wavekube

Kubernetes operator for NVIDIA Aerial GPU-accelerated 5G/6G RAN.

Cloud-native gNodeB lifecycle management, security compliance, and observability — the missing platform layer between NVIDIA's Aerial SDK and production telecom deployments.

## What is this?

wavekube bridges the gap between NVIDIA's raw Aerial CUDA-Accelerated RAN SDK and production Kubernetes deployments. No one else has built this.

**Custom Resources:**
- `GNodeB` — GPU-accelerated 5G/6G base station with cuPHY configuration, GPU scheduling, RDMA, and O-RAN fronthaul
- `RANPipeline` — Signal processing pipelines built with Aerial Framework, deployed as K8s workloads
- `RANSecurityPolicy` — Image scanning, runtime monitoring, network isolation, and compliance enforcement for RAN workloads

## Quick Start

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# Deploy operator via Helm
helm install wavekube deploy/helm/wavekube/ -n wavekube-system --create-namespace

# Create a gNodeB
kubectl apply -f config/samples/gnodeb_sample.yaml

# Check status
kubectl get gnb
```

## Architecture

```
┌─────────────────────────────────────────────────┐
│                 wavekube operator                │
│                                                  │
│  ┌──────────┐  ┌────────────┐  ┌─────────────┐ │
│  │  GNodeB  │  │ RANPipeline│  │  Security   │ │
│  │Reconciler│  │ Reconciler │  │  Reconciler │ │
│  └────┬─────┘  └─────┬──────┘  └──────┬──────┘ │
└───────┼──────────────┼────────────────┼─────────┘
        │              │                │
   ┌────▼────┐   ┌────▼─────┐   ┌─────▼──────┐
   │Aerial   │   │ Aerial   │   │  Falco     │
   │CUDA RAN │   │Framework │   │  Trivy     │
   │Container│   │ Pipeline │   │  Cilium    │
   └────┬────┘   └──────────┘   └────────────┘
        │
   ┌────▼────┐
   │NVIDIA   │
   │GPU + RU │
   └─────────┘
```

## Prerequisites

- Kubernetes 1.28+
- NVIDIA GPU Operator installed
- NVIDIA Network Operator (for RDMA/GPUDirect)
- Helm 3.x

## Testing

```bash
# Unit tests
make test-unit

# Integration tests (requires envtest)
make test-integration

# E2E tests (requires kind cluster)
make kind-setup
make test-e2e

# All tests
make test
```

## License

Apache 2.0
