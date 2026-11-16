# DRA examples

Request NPUs with ResourceClaims and verify the allocation.

## Before you start

Use Kubernetes 1.34 or later and
[enable DRA through the NPU Operator](https://docs.rbln.ai/latest/software/system_management/kubernetes/npu_dra_driver.html).
Confirm that the selected DeviceClass and matching device inventory exist.
Use examples from the same release as your deployment.

| Goal | Guide |
| --- | --- |
| Run a first DRA workload | [One Pod, one NPU](single-pod-single-npu.md) |
| Allocate multiple NPUs to one workload | [One Pod, two NPUs](single-pod-double-npu.md) |
| Give each workload its own NPU | [Two Pods, one NPU each](two-pods-one-npu-each.md) |
| Use a partitioned NPU | [One Pod, one VF](single-pod-npu-vf4.md) |
| Attach an NPU to a virtual machine | [KubeVirt VM passthrough](kubevirt-vm-passthrough.md) |

Container examples use `npu.rebellions.ai` for whole NPUs or a VF-specific class.
VM examples use the corresponding `vfio-npu*` class. See the
[device class table](../README.md#choose-a-device-class).

Each guide creates a test namespace and includes cleanup.

[Back to the README](../README.md)
