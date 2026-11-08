# KubeVirt VM with an NPU passed through via DRA

For VM passthrough, usable Rebellions PCI functions (vendor `0x1eff`, class
`0x120000`) must be bound to `vfio-pci`. With SR-IOV, the PF stays on the
native `rebellions` driver and only its VFs bind to `vfio-pci`.
The kubelet plugin discovers those devices by scanning
`/sys/bus/pci/devices` and publishes them in the node's ResourceSlice as
`vfio-<pci-address>` devices with `type: "vfio"`, alongside:

- `resource.kubernetes.io/pciBusID` — the PCI address KubeVirt uses to build
  the passthrough `<hostdev>`.
- `resource.kubernetes.io/pcieRoot`, `resource.kubernetes.io/numaNode` — for
  alignment constraints (`numaNode` is omitted when sysfs reports none).
- `iommuGroup`, `pciDeviceID`, `productName`, `vf` — driver-specific
  attributes for selection and debugging (`productName` is only set for
  known models).
- `partitionMode`, `numVFs`, `parentPCIBusID` — select `none`, `vf1` or
  `vf4` and identify a VF's parent. Hosting PFs and unsupported or unreadable
  VF partition modes are excluded.

When a claim for a vfio device is prepared, the plugin injects
`/dev/vfio/vfio` and `/dev/vfio/<iommu group>` into KubeVirt's virt-launcher
through CDI, owned by uid/gid 107 so QEMU can open them, and publishes
per-claim device metadata files (KEP-5304 style) that virt-launcher reads to
discover the PCI address. The RBLN runtime (UMD) mounts used for container
workloads are *not* applied to vfio claims.

The `DeviceClass` named `vfio-npu.rebellions.ai` selects only whole PFs.
To request one VF from a PF in VF4 mode, use the following `exactly` block
(VF1 uses `vfio-npu-vf1.rebellions.ai`):

```yaml
deviceClassName: vfio-npu-vf4.rebellions.ai
count: 1
```

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
          # NOTE: in KubeVirt v1.9.x ClaimRequest is embedded inline —
          # claimName/requestName go directly on the hostDevice entry
          # (no claimRequest: wrapper).
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

## Notes

- A node starting without usable devices keeps scanning. The default
  `DEVICE_SCAN_INTERVAL` is `5s`; successful scans publish inventory changes.
- A failed scan preserves the last complete native/VFIO inventory. New
  Prepare calls still check live PCI binding, profile and IOMMU ownership.
- Devices without an exclusive IOMMU group are omitted. The exclusion reason
  is logged at startup or when a device leaves the inventory.
- Passthrough devices are not requestable through the `rebellions.ai/npu`
  extended resource — only through an explicit ResourceClaim.

## SR-IOV hardware validation

After configuring one or four VFs per PF and verifying their VFIO bindings,
use the corresponding VFIO class in the template above. Create two VMs
with different names referencing that template so each receives its own claim. Verify both VMs reach Running and
inspect their allocated PCI addresses in ResourceSlices and the guests.
The two claims must have distinct VF BDFs and distinct IOMMU groups.

On the host, for each allocated VF:

```bash
VF_BDF=0000:17:00.1  # replace with the allocated VF's BDF
readlink "/sys/bus/pci/devices/$VF_BDF/driver"
readlink "/sys/bus/pci/devices/$VF_BDF/physfn"
readlink "/sys/bus/pci/devices/$VF_BDF/iommu_group"
ls -1 "/sys/bus/pci/devices/$VF_BDF/iommu_group/devices"
```

Expect `vfio-pci`, the native parent PF, and a group listing only that VF.
Before testing a different profile, terminate both VMs and release their
claims, then change the host layout and driver bindings. Wait for the new
ResourceSlice inventory before recreating the VMs with the new profile
class; old VF identities must not satisfy the new profile.
