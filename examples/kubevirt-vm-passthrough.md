# KubeVirt VM with an NPU passed through via DRA

On nodes with the `vm-passthrough` workload config, the NPU operator's
vfio-manager binds Rebellions NPUs (PCI vendor `0x1eff`, class `0x120000`) to
the `vfio-pci` driver. The kubelet plugin discovers those devices by scanning
`/sys/bus/pci/devices` and publishes them in the node's ResourceSlice as
`vfio-<pci-address>` devices with `type: "vfio"`, alongside:

- `resource.kubernetes.io/pciBusID` — the PCI address KubeVirt uses to build
  the passthrough `<hostdev>`.
- `resource.kubernetes.io/pcieRoot`, `resource.kubernetes.io/numaNode` — for
  alignment constraints (`numaNode` is omitted when sysfs reports none).
- `iommuGroup`, `pciDeviceID`, `productName`, `vf` — driver-specific
  attributes for selection and debugging (`productName` is only set for
  known models).

When a claim for a vfio device is prepared, the plugin injects
`/dev/vfio/vfio` and `/dev/vfio/<iommu group>` into KubeVirt's virt-launcher
through CDI, owned by uid/gid 107 so QEMU can open them, and publishes
per-claim device metadata files (KEP-5304 style) that virt-launcher reads to
discover the PCI address. The RBLN runtime (UMD) mounts used for container
workloads are *not* applied to vfio claims.

The `DeviceClass` named `vfio-npu.rebellions.ai` selects only passthrough
devices.

## Prerequisites

- Kubernetes v1.34 or later (`resource.k8s.io/v1`).
- KubeVirt v1.9.0 or later with the `HostDevicesWithDRA` feature gate
  enabled.
- RBLN NPU Operator with `workloadType: vm-passthrough` and
  `draKubeletPlugin.enabled: true`. `sandboxDevicePlugin` must stay disabled:
  the two advertise the same vfio devices, so the operator rejects a policy
  that enables both.
- IOMMU enabled on the host (VM-based nodes need a vIOMMU).

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

- On vm-passthrough nodes the kubelet plugin crash-loops until vfio-manager
  binds the first device — this is expected during bring-up.
- vfio binding happens at runtime, so the plugin rescans every 30 seconds;
  newly bound devices can take up to one interval to appear in the
  ResourceSlice.
- Devices without an IOMMU group cannot be passed through and are silently
  not advertised.
- Passthrough devices are not requestable through the `rebellions.ai/npu`
  extended resource — only through an explicit ResourceClaim.
