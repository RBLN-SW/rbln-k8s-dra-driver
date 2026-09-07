# RBLN DRA Driver for NPUs

This repository implements a Kubernetes [Dynamic Resource Allocation (DRA)](https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/) driver for Rebellions NPUs. DRA is the modern standard for allocating resources such as NPUs in Kubernetes. It fully replaces Device Plugin in terms of functionality and provides additional capabilities.

## Installation

### Prerequisites

- Kubernetes v1.34 or later (not tested on older versions)
- [RBLN NPU Operator](https://github.com/RBLN-SW/rbln-npu-operator) v0.2.1 or
  later (VM passthrough requires an operator version with DRA vfio
  passthrough support)
- CDI must be enabled in the container runtime

### Install with Helm

```bash
helm repo add rebellions https://rbln-sw.github.io/charts/
helm repo update
helm install k8s-dra-driver-npu rebellions/k8s-dra-driver-npu
```

The NPU operator can also deploy this driver itself
(`draKubeletPlugin.enabled: true` in the `RBLNClusterPolicy`); in that case
do not install this chart separately.

## Logging

Both binaries (`npu-kubelet-plugin`, `webhook`) log structured JSON to stdout by
default and are configured through environment variables only:

| Variable | Values | Default |
|---|---|---|
| `RBLN_DRA_DRIVER_LOG_LEVEL` | `error`, `warning` (or `warn`), `info`, `debug`, `trace` | `info` |
| `RBLN_DRA_DRIVER_LOG_FORMAT` | `json`, `text` | `json` |

Invalid values never kill the process: the offending variable falls back to its
default and a single warning with a `fallback` key is logged. `debug` adds a
`caller` field; `trace` additionally includes gRPC request/response payload
dumps from the DRA kubelet-plugin helper and admission payloads from the
webhook — do not run production at `trace`. The `text` format is intended for
local debugging only.

The level also drives the verbosity of the Kubernetes libraries: their klog
`V(n)` records are folded into the same stream, with `debug` admitting up to
`V(4)` and `trace` up to `V(8)`. One exception to the single-stream rule: the
`npu-kubelet-plugin` links the NPU userspace library, which logs through glog,
so a few RSD-group messages arrive on **stderr in glog's own text format**
rather than as JSON on stdout.

With the Helm chart, set these through the shared `logging` values:

```bash
helm install k8s-dra-driver-npu rebellions/k8s-dra-driver-npu \
  --set logging.level=debug --set logging.format=json
```

### Record schema

JSON records use the same field names and types as kubelet's own JSON logs, so
both can be parsed by one collector configuration and indexed into one field
mapping:

| Key | Type | Notes |
|---|---|---|
| `ts` | number | Epoch milliseconds, matching kubelet's component-base encoder. The `text` format uses RFC3339Nano instead. |
| `level` | string | `error`, `warn`, `info`, `debug`, `trace` — a closed vocabulary. |
| `v` | number | klog verbosity depth, present below `warn`. `info` is `0`, `debug` `4`, `trace` up to `8`. |
| `msg` | string | |
| `err` | string | Present on failures. |
| `caller` | string | `dir/file.go:line`, added at `debug` and below only. |
| `impact` | string | On the warn/error records where the driver degrades a workload instead of failing it, states what breaks. |

Because `level` buckets every klog depth into five names, `v` is what
distinguishes a `V(5)` record from a `V(7)` one — filter on it to make `trace`
readable, e.g. `jq 'select(.v <= 5)'`.

Request-scoped records carry correlation keys: `requestID` and `method` from the
DRA kubelet-plugin helper, plus `claimUID`, `claimNamespace` and `claimName` on
everything a claim's prepare or unprepare touches. An operator holding a pending
pod can go from the claim's namespace/name straight to the driver's records for
it. Webhook records carry `requestUID`, `resource`, `namespace` and `name` for
the reviewed object.

Each binary reports its build in the first record it writes
(`Starting npu-kubelet-plugin` / `Starting webhook server`) under `version`,
stamped from the release tag. A plain `make cmds` with no `VERSION` reports
`devel`.

### What to look for

| Symptom | Look for |
|---|---|
| Plugin may not have come up | `Driver started` is the last startup record; without it the plugin never finished registering |
| Pods stay `Pending`, no devices offered | `No NPU devices found on this node` (warn), or the `Driver started` `deviceCount` (`npuDeviceCount` / `vfioDeviceCount` split it by kind) |
| VMs stay `Pending`, no passthrough device offered | `Failed to enumerate vfio-pci NPU devices` (error) or `Skipping vfio-pci NPU without an IOMMU group` (warn); a passthrough-only node also reports `rbln-smi enumeration unavailable, continuing with vfio-pci devices only` |
| Passthrough devices came or went without a restart | `vfio-pci NPU devices changed, republishing resources` with `added` / `removed`, or `Failed to rescan vfio-pci NPU devices` (error) |
| Pod stuck in `ContainerCreating` | `Failed to prepare devices for claim` (error) with `err` and the claim keys |
| Multi-NPU job runs but peers cannot talk | `RSD group creation returned no device path` (error) — the pod is Ready but degraded |
| Container cannot see `/dev/rsd0` or the NPU | `Wrote CDI spec for claim` at `debug`, which lists the injected `deviceNodes` |
| Container cannot find the userspace library | `Wrote common CDI spec with runtime edits` at `debug`, with `mounts`/`hooks` counts |
| Devices seem leaked after a pod is gone | `Unprepared devices for claim`, or `No prepared state for claim` at `debug` |
| Topology-aware allocation behaves oddly | `Ignoring unparseable NUMA node` (warn), `Device reports no NUMA affinity` at `debug` |
| Cross-driver `matchAttribute` on `pcieRoot` never matches | `Ignoring unresolvable PCIe root` (warn); the attribute is resolved from sysfs, so check `/sys` is visible to the plugin |
| A VM's claim fails with "metadata directory ... is owned by claim" | Another live claim with the same name holds the KubeVirt metadata directory; `Failed to roll back KubeVirt device metadata` (warn) explains a leftover from an earlier failed prepare |

## Usage Examples

See `examples/README.md` for usage examples.
