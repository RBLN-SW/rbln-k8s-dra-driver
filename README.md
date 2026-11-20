# RBLN DRA Driver

**Allocate Rebellions NPUs with Kubernetes ResourceClaims.**

[Get started](#get-started) · [Guides](#guides) · [Examples](examples/README.md) · [Releases](https://github.com/RBLN-SW/rbln-k8s-dra-driver/releases)

The DRA driver lets workloads request NPUs by count and device attributes.
It supports container workloads and KubeVirt VM passthrough, including SR-IOV
virtual functions on prepared nodes.

## Get started

1. Use Kubernetes 1.34 or later and [install the NPU Operator](https://docs.rbln.ai/latest/software/system_management/kubernetes/install_npu_operator.html).
2. [Enable DRA](https://docs.rbln.ai/latest/software/system_management/kubernetes/npu_dra_driver.html) with the required runtime configuration.
3. [Run a Pod with one NPU](examples/single-pod-single-npu.md) and check its ResourceClaim.

If DRA is already enabled, start with the example.

The operator rejects DRA configurations that also enable the Device Plugin or
Sandbox Device Plugin. Prepare and migrate workloads before changing allocators.
For a separate component deployment, use the [standalone chart guide](docs/guides/deploy-standalone.md).

## Guides

| I want to… | Guide |
| --- | --- |
| Request one NPU | [Create a claim and Pod](examples/single-pod-single-npu.md) |
| Select different native requests in an init and app container | [Experimental RSD request views](docs/guides/rsd-request-views.md) |
| Request multiple NPUs | [Allocate two NPUs](examples/single-pod-double-npu.md) |
| Select devices by product or topology | [Set allocation conditions](https://docs.rbln.ai/latest/software/system_management/kubernetes/tutorials/configure_dra_allocation.html) |
| Request a virtual function | [Use an SR-IOV VF](examples/single-pod-npu-vf4.md) |
| Attach an NPU to a VM | [Use DRA with KubeVirt](examples/kubevirt-vm-passthrough.md) |
| Investigate an unallocated claim or stuck Pod | [Diagnose allocation](docs/guides/troubleshoot-allocation.md) |
| Change logging or plugin settings | [Configuration reference](docs/reference/configuration.md) |

## Choose a device class

| Device | Container workload | VM passthrough |
| --- | --- | --- |
| Whole NPU | `npu.rebellions.ai` | `vfio-npu.rebellions.ai` |
| VF from a VF1 partition | `npu-vf1.rebellions.ai` | `vfio-npu-vf1.rebellions.ai` |
| VF from a VF4 partition | `npu-vf4.rebellions.ai` | `vfio-npu-vf4.rebellions.ai` |

A request with `count: 1` allocates one device. For a VF4 class, that means one VF.
Use explicit ResourceClaims to follow the examples; extended-resource aliases
have additional Kubernetes feature requirements.

## Development

See [Contributing](CONTRIBUTING.md) to build and test the driver.
Use docs and examples from the Git tag matching your deployed driver.

## Support

Report bugs and feature requests in [GitHub issues](https://github.com/RBLN-SW/rbln-k8s-dra-driver/issues).
Include the Pod and ResourceClaim status when reporting allocation problems.

[License](LICENSE)
