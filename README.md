# RBLN DRA Driver for NPUs

This repository implements a Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) driver for Rebellions NPUs. DRA is the modern standard for allocating resources such as NPUs in Kubernetes. It fully replaces Device Plugin in terms of functionality and provides additional capabilities.

## Installation

### Prerequisites

- Kubernetes v1.34 or later (not tested on older versions)
- [RBLN NPU Operator](https://github.com/RBLN-SW/rbln-npu-operator) v0.2.1 or
  later (VM passthrough requires an operator version with DRA vfio
  passthrough support)
- CDI must be enabled in the container runtime

### Install with Helm

```bash
helm repo add rebellions https://rbln-sw.github.io/charts/
helm repo update
helm install k8s-dra-driver-npu rebellions/k8s-dra-driver-npu
```

The NPU operator can also deploy this driver itself
(`draKubeletPlugin.enabled: true` in the `RBLNClusterPolicy`); in that case
do not install this chart separately.

## Usage Examples

See `examples/README.md` for usage examples.
