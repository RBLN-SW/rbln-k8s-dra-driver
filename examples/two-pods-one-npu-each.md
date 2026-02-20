# Two Pods Requesting One NPU Each

This example creates two pods, each with its own claim template requesting one NPU.

## Apply

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: npu-example-two-pods
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: npu-example-two-pods
  name: pod0-npu
spec:
  spec:
    devices:
      requests:
      - name: npu
        exactly:
          deviceClassName: npu.rebellions.ai
          count: 1
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: npu-example-two-pods
  name: pod1-npu
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
  namespace: npu-example-two-pods
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
    resourceClaimTemplateName: pod0-npu
---
apiVersion: v1
kind: Pod
metadata:
  namespace: npu-example-two-pods
  name: pod1
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
    resourceClaimTemplateName: pod1-npu
EOF
```

## Verify

```bash
kubectl -n npu-example-two-pods get pod,resourceclaim
kubectl -n npu-example-two-pods describe pod pod0
kubectl -n npu-example-two-pods describe pod pod1
```

## Cleanup

```bash
kubectl delete namespace npu-example-two-pods
```
