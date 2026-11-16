# Deploy the component chart

Use this guide when you manage this component separately from the NPU Operator.

## Before you start

- Kubernetes 1.34 or later, prepared NPU nodes, and a CDI-enabled runtime.
- For container workloads, an installed RBLN NPU driver and
  [RBLN Container Toolkit](https://docs.rbln.ai/latest/software/system_management/container_toolkit.html)
  with its runtime CDI spec generated.
- Helm and `kubectl` configured for the target cluster.
- A checkout of the release you intend to deploy.

The NPU Operator must not also deploy the DRA Driver on these nodes. Disable
Device Plugin and Sandbox Device Plugin allocation for the same devices.
Plan any allocator change before installing this chart.

## 1. Choose the image and settings

Select the image tag for your release and review the chart's
[values](../../deployments/helm/k8s-dra-driver-npu/values.yaml).
Confirm that its node selector and tolerations match the prepared NPU nodes.
Use a values file for any site-specific overrides.

## 2. Install

Run from the repository root. Replace the image tag before running:

```bash
export COMPONENT_IMAGE_TAG='<image-tag>'
helm upgrade --install rbln-dra-driver \
  ./deployments/helm/k8s-dra-driver-npu \
  --namespace rbln-dra-driver --create-namespace \
  --set-string image.repository=rebellions/k8s-dra-driver-npu \
  --set-string image.tag="$COMPONENT_IMAGE_TAG" \
  --wait --timeout 5m
```

Add `-f <values-file>` if you prepared custom settings.

## 3. Verify

```bash
kubectl get daemonsets,pods -n rbln-dra-driver
kubectl get deviceclasses
kubectl get resourceslices
```

Expect Ready Pods on the selected NPU nodes and the expected device inventory.
Then [run a Pod with one NPU](../../examples/single-pod-single-npu.md).

## Remove this deployment

Stop workloads using this allocator and release their device allocations first.
Then remove only this Helm release:

```bash
helm uninstall rbln-dra-driver -n rbln-dra-driver
```

Return to the [component guides](../../README.md#guides).
