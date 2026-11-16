# Diagnose a DRA allocation

Use this guide when a ResourceClaim is unallocated or its Pod cannot start.
Use the workload namespace for claims; DeviceClasses and ResourceSlices are
cluster-scoped.

## 1. Inspect the Pod and claim

```bash
kubectl describe pod <pod-name> -n <workload-namespace>
kubectl get resourceclaims -n <workload-namespace>
kubectl get resourceclaim <claim-name> -n <workload-namespace> -o yaml
kubectl get deviceclasses
kubectl get resourceslices -o yaml
```

For a ResourceClaimTemplate, find the generated claim name from the Pod.
Check `status.allocation` on that claim, then compare its request with the
selected DeviceClass and the devices published for eligible nodes.

| Symptom | Next check |
| --- | --- |
| Claim is not allocated | DeviceClass, count, selectors, available devices, and Pod scheduling constraints |
| Expected devices are absent from ResourceSlices | Driver readiness, plugin logs, native NPU health, and the node's partition/workload mode |
| Claim is allocated but Pod stays in `ContainerCreating` | Plugin preparation logs on the selected node |
| VFIO devices are absent | `vfio-pci` binding and one PCI function per usable IOMMU group |
| Pod runs but its devices or runtime libraries are missing | CDI runtime configuration and the generated claim/runtime specifications |

Use a [class matching your device mode](../../README.md#choose-a-device-class).
For partition changes, follow the [recovery steps below](#3-recover-and-verify).

## 2. Trace preparation on the selected node

```bash
kubectl get pods -n rbln-system -o wide
kubectl logs <dra-plugin-pod> -n rbln-system --all-containers --tail=200
```

For the standalone guide, use namespace `rbln-dra-driver`. Select the plugin Pod
on the workload's node and find the claim's name or UID in its logs.

| Log record | Meaning / action |
| --- | --- |
| `Driver started` | Plugin initialization completed; inspect its device counts |
| `Failed to prepare devices for claim` | Read `err` and claim identifiers; failed RSD group creation prevents startup |
| `Device scan failed, keeping last inventory` | Discovery failed; inspect the error instead of assuming the published inventory is current |
| `NPU is unhealthy, excluding from new allocations` | Check the named device's status; existing claims keep their assignments |
| `Wrote CDI spec for claim` at debug | Inspect `deviceNodes` when injected devices are missing |
| `Unprepared devices for claim` | The plugin completed claim cleanup |

[Enable debug logs](../reference/configuration.md#change-logging) if the normal
stream does not contain enough detail. Preserve active claims and their state
while diagnosing; deleting them can affect workloads that share the allocation.

## 3. Recover and verify

Native NPUs observed unhealthy are excluded from new allocations. The driver
does not evict running Pods or reallocate existing claims. To move a workload
to another NPU, stop the affected consumers and recreate the workload with a
new claim. Resume use of a repaired NPU after it is healthy and appears in
ResourceSlices again.

Before changing partitions or driver bindings, stop affected workloads and
wait for their allocations to be released. Wait for ResourceSlices to reflect
the new layout, then create new claims; do not reuse pre-change claims.

Run the [one-NPU example](../../examples/single-pod-single-npu.md).
Expect an allocated claim, a Ready Pod, and accessible NPU and RSD device nodes.
For a VM, use the [passthrough example](../../examples/kubevirt-vm-passthrough.md)
and confirm the device inside the guest.

[Back to the guides](../../README.md#guides)
