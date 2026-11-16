# Attach an NPU to a KubeVirt VM with DRA

Create a ResourceClaim and let a VM consume the allocated NPU. This example
uses a whole device through `vfio-npu.rebellions.ai`.

For a VF, choose `vfio-npu-vf1.rebellions.ai` or `vfio-npu-vf4.rebellions.ai`.
One requested device means one VF. Prepare the host's partitions and VFIO
bindings before creating the claim.

## Prerequisites

- Kubernetes v1.34 or later (`resource.k8s.io/v1`).
- KubeVirt v1.9.0 or later with the `HostDevicesWithDRA` feature gate
  enabled.
- The RBLN DRA kubelet plugin deployed and publishing the intended VFIO
  devices in ResourceSlices. Use a single allocator for each PCI function;
  do not advertise the same functions through another device plugin.
- IOMMU enabled on the host (VM-based nodes need a vIOMMU), with one PCI
  function per allocated IOMMU group. Shared groups are not advertised.

## Apply

```bash
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: Namespace
metadata:
  name: npu-example-passthrough
---
apiVersion: resource.k8s.io/v1
kind: ResourceClaimTemplate
metadata:
  namespace: npu-example-passthrough
  name: npu-passthrough
spec:
  spec:
    devices:
      requests:
      - name: npu
        exactly:
          deviceClassName: vfio-npu.rebellions.ai
          # To pin a specific model:
          # selectors:
          # - cel:
          #     expression: 'device.attributes["npu.rebellions.ai"].productName == "RBLN-CA25"'
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  namespace: npu-example-passthrough
  name: vm-with-npu
spec:
  runStrategy: Always
  template:
    spec:
      resourceClaims:
      - name: npu-claim
        resourceClaimTemplateName: npu-passthrough
      domain:
        cpu:
          cores: 4
        memory:
          guest: 8Gi
        devices:
          disks:
          - name: rootdisk
            disk: {bus: virtio}
          - name: cloudinit
            disk: {bus: virtio}
          interfaces:
          - name: default
            masquerade: {}
          hostDevices:
          - name: npu0
            claimName: npu-claim
            requestName: npu
      networks:
      - name: default
        pod: {}
      volumes:
      - name: rootdisk
        containerDisk:
          image: quay.io/containerdisks/ubuntu:24.04
      - name: cloudinit
        cloudInitNoCloud:
          userData: |
            #cloud-config
            password: ubuntu
            chpasswd: { expire: false }
            packages: [pciutils]
EOF
```

## Verify

```bash
# Passthrough NPUs appear as vfio-<pci-address> devices per node pool
kubectl get resourceslices -o jsonpath='{range .items[*]}{.spec.pool.name}{": "}{.spec.devices[*].name}{"\n"}{end}'

# VM running and claim allocated
kubectl -n npu-example-passthrough get vm,vmi,resourceclaim

# Inside the guest (log in with ubuntu / ubuntu)
virtctl -n npu-example-passthrough console vm-with-npu
lspci -nn | grep 1eff
```

## Cleanup

```bash
kubectl delete namespace npu-example-passthrough
```

## Next steps

Install a compatible RBLN driver in the guest to run NPU workloads. For VF
passthrough, repeat the example with a VF-specific DeviceClass and prepared VFs.
Stop the VM and release its claim before changing host partitions or bindings.

[Back to the examples](README.md)
