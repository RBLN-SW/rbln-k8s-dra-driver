# Usage Examples

This directory contains examples for using the DRA driver.
The Helm chart creates `npu.rebellions.ai` for physical container NPUs,
`npu-vf1.rebellions.ai` and `npu-vf4.rebellions.ai` for the respective SR-IOV profiles,
and `vfio-npu.rebellions.ai` for VM passthrough (vfio-pci) devices.

## Example Scenarios

- [Single pod requesting an SR-IOV VF](single-pod-npu-vf4.md)
- [Single pod requesting one NPU](single-pod-single-npu.md)
- [Single pod requesting two NPUs](single-pod-double-npu.md)
- [Two pods requesting one NPU each](two-pods-one-npu-each.md)
- [KubeVirt VM with an NPU passed through via DRA](kubevirt-vm-passthrough.md)
