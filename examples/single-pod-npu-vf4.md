# Single Pod Requesting an SR-IOV VF

This example allocates one VF from a physical function (PF) configured with
four VFs. The host must expose these VFs through the native NPU driver, and
the DRA kubelet plugin must publish them in ResourceSlices. Use a single
allocator for these devices; do not advertise the same NPUs through another
device plugin at the same time.

Verify the VF DeviceClass and the published device inventory before applying
the workload:

```bash
kubectl get deviceclass npu-vf4.rebellions.ai
kubectl get resourceslices -o yaml
```

Check that the intended node's inventory contains VFs with
`partitionMode: vf4` and `numVFs: 4`. One VF is one DRA device; `count: 2` requests two VFs.
With one VF per PF, use the `npu-vf1.rebellions.ai` class instead.

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
