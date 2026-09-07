# Usage Examples

This directory contains examples for using the DRA driver.
The Helm chart creates two `DeviceClass` objects: `npu.rebellions.ai` for
container workloads and `vfio-npu.rebellions.ai` for VM passthrough
(vfio-pci) devices.

## Example Scenarios

- [Single pod requesting one NPU](single-pod-single-npu.md)
- [Single pod requesting two NPUs](single-pod-double-npu.md)
- [Two pods requesting one NPU each](two-pods-one-npu-each.md)
- [KubeVirt VM with an NPU passed through via DRA](kubevirt-vm-passthrough.md)
