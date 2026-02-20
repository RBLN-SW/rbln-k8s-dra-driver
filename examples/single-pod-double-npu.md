# Single Pod Requesting Two NPUs

This example creates one pod that requests two NPU devices via a single DRA claim.

## Apply

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: npu-example-double
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: npu-example-double
  name: double-npu
spec:
  spec:
    devices:
      requests:
      - name: npus
        exactly:
          deviceClassName: npu.rebellions.ai
          count: 2
---
apiVersion: v1
kind: Pod
metadata:
  namespace: npu-example-double
  name: pod0
spec:
  containers:
  - name: ctr0
    image: ubuntu:22.04
    command: ["bash", "-c"]
    args: ["trap 'exit 0' TERM; sleep 9999 & wait"]
    resources:
      claims:
      - name: npus
  resourceClaims:
  - name: npus
    resourceClaimTemplateName: double-npu
EOF
```

## Verify

```bash
kubectl -n npu-example-double get pod,resourceclaim
kubectl -n npu-example-double describe pod pod0
```

## Cleanup

```bash
kubectl delete namespace npu-example-double
```
