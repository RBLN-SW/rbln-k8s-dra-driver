# Single Pod Requesting an SR-IOV VF

This example allocates one VF from a PF partitioned into four VFs. Enable
`partitionManager` and `draKubeletPlugin` in the operator, disable the ordinary
device-plugin on these nodes, and use builds containing SR-IOV DRA support.

Set the desired layout and wait for partition-manager to finish:

```bash
kubectl label node <node> rebellions.ai/npu.partition=vf4 --overwrite
kubectl wait node <node> --for=condition=NPUPartitionReady --timeout=5m
kubectl get deviceclass npu-vf4.rebellions.ai
```

The operator gates DRA startup on partition readiness. One VF is one DRA
device; `count: 2` requests two VFs. For `vf1`, use the `vf1` label and
`npu-vf1.rebellions.ai` class instead.

## Apply

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: npu-example-vf4
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: npu-example-vf4
  name: single-vf4
spec:
  spec:
    devices:
      requests:
      - name: npu
        exactly:
          deviceClassName: npu-vf4.rebellions.ai
          count: 1
---
apiVersion: v1
kind: Pod
metadata:
  namespace: npu-example-vf4
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
    resourceClaimTemplateName: single-vf4
EOF
```

## Verify

```bash
kubectl -n npu-example-vf4 get pod,resourceclaim
kubectl -n npu-example-vf4 describe pod pod0
kubectl -n npu-example-vf4 get resourceclaim -o yaml
kubectl -n npu-example-vf4 exec pod0 -- rbln-smi -j -g
```

The allocation should name a VF device (for example `rbln0-0-vf4`). Its
ResourceSlice attributes should show `vf: true`, `partitionMode: vf4`,
`numVFs: 4`, and the parent's `parentPCIBusID`. In the container, verify the
allocated VF node (`deviceNodeName`, for example `/dev/rbln0-0`) and `/dev/rsd0`
are present. The profile suffix is part of the DRA identity, not the node path.

For clusters with the `DRAExtendedResource` bridge enabled, a Pod may instead
request `limits: { rebellions.ai/npu-vf4: 1 }`. That alias selects the same VF
class; do not combine both request styles for the same allocation.

## Cleanup

```bash
kubectl delete namespace npu-example-vf4
```
