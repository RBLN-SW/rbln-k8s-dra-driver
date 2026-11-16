# Single Pod Requesting One NPU

Request one whole NPU with a ResourceClaimTemplate.

## Before you start

Use Kubernetes 1.34 or later and [enable DRA](https://docs.rbln.ai/latest/software/system_management/kubernetes/npu_dra_driver.html).
Confirm that `npu.rebellions.ai` exists and ResourceSlices contain an available NPU.
This example creates the `npu-example-single` test namespace.

## Apply

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: npu-example-single
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: npu-example-single
  name: single-npu
spec:
  spec:
    devices:
      requests:
      - name: npu
        exactly:
          deviceClassName: npu.rebellions.ai
          count: 1
---
apiVersion: v1
kind: Pod
metadata:
  namespace: npu-example-single
  name: pod0
spec:
  containers:
  - name: ctr0
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: npu
  resourceClaims:
  - name: npu
    resourceClaimTemplateName: single-npu
EOF
```

## Verify

```bash
kubectl -n npu-example-single wait pod/pod0 --for=condition=Ready --timeout=5m
kubectl -n npu-example-single get pod,resourceclaim
kubectl -n npu-example-single describe pod pod0
kubectl -n npu-example-single exec pod0 -- sh -c 'ls /dev/rbln* /dev/rsd0'
```

Expect a Ready Pod, an allocated ResourceClaim, and the allocated NPU and RSD
device nodes. If the Pod remains Pending,
read its scheduling events and inspect the generated claim with
`kubectl -n npu-example-single get resourceclaim -o yaml`.
For preparation or device-injection errors, follow the
[allocation diagnosis guide](../docs/guides/troubleshoot-allocation.md).

## Cleanup

```bash
kubectl delete namespace npu-example-single
```
