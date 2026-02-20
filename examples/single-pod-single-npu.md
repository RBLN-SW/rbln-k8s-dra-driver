# Single Pod Requesting One NPU

This example creates one pod with one DRA claim.

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
kubectl -n npu-example-single get pod,resourceclaim
kubectl -n npu-example-single describe pod pod0
```

## Cleanup

```bash
kubectl delete namespace npu-example-single
```
