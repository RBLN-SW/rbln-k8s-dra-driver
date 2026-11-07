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

The driver identity is fixed to `npu.rebellions.ai`.

## SR-IOV resources

REBEL VFs in `vf1` and `vf4` mode can be allocated through DRA. Deploy builds
of the DRA driver, NPU operator, and device-config-manager that include this
support together. VF creation remains the device-config-manager's responsibility.

| DeviceClass | Extended resource | Devices selected |
|---|---|---|
| `npu.rebellions.ai` | `rebellions.ai/npu` | Unpartitioned physical NPUs |
| `npu-vf1.rebellions.ai` | `rebellions.ai/npu-vf1` | VFs from PFs with one VF |
| `npu-vf4.rebellions.ai` | `rebellions.ai/npu-vf4` | VFs from PFs with four VFs |
| `vfio-npu.rebellions.ai` | — | Unpartitioned vfio-pci PFs |
| `vfio-npu-vf1.rebellions.ai` | — | vfio-pci VFs from PFs with one VF |
| `vfio-npu-vf4.rebellions.ai` | — | vfio-pci VFs from PFs with four VFs |

A claim for one `npu-vf4.rebellions.ai` device receives **one VF**, not four.
The same applies to `vfio-npu-vf4.rebellions.ai`. `count` always counts devices;
four VF4 devices are not converted into a PF allocation and need not share a
parent. Model and memory requirements can further restrict a class through
selectors. Different profiles are never substituted implicitly.
PFs hosting VFs are excluded because enabling SR-IOV removes them from the
compute topology. Unpartitioned PFs and both VF modes can coexist on a node.
This driver publishes VFs as `type: npu-vf`, so an older PF class selecting only `type: npu`
cannot allocate a partition to a full-NPU request. The Helm chart and operator
must select `type: npu-vf` in their VF classes. VF requests remain pending
until both the VF class and driver use this contract.
The extended-resource aliases require the cluster's `DRAExtendedResource`
feature gate; explicit ResourceClaims do not require that bridge. See the
[Kubernetes 1.35 DRA documentation](https://v1-35.docs.kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/#extended-resource-allocation-by-dra).

### Discovery and allocation

Device classification uses PCI sysfs:

1. Read native compute inventory from `rbln-smi` through `rblnlib-go`, and
   vfio-pci devices with IOMMU groups from PCI sysfs.
2. Read `/sys/bus/pci/devices/<BDF>/physfn` to identify a VF and its parent.
   Read the parent's `sriov_numvfs` to distinguish `vf1` and `vf4`.
3. Exclude hosting PFs, unsupported VF counts, and unreadable/inconsistent
   SR-IOV state. Publish each usable VF as one device in the ResourceSlice.
4. Prepare the allocated VF's own PCI address in the RSD group and inject its
   `/dev/rblnN-M` node through CDI, with the existing runtime and RDS edits.
   RSD group creation errors fail preparation; they never substitute the
   host's shared `/dev/rsd0`.

A missing PCI entry is excluded rather than treated as an unpartitioned PF.
An existing PF without SR-IOV attributes remains supported as a physical NPU.

Container devices expose these attributes under the fixed driver domain
`npu.rebellions.ai`:

| Attribute | Type | PF | VF |
|---|---|---|---|
| `type` | string | `npu` | `npu-vf` |
| `vf` | bool | `false` | `true` |
| `partitionMode` | string | `none` | `vf1` or `vf4` |
| `numVFs` | int | `0` | Parent PF's enabled VF count |
| `parentPCIBusID` | string | omitted | Parent PF BDF |

`resource.kubernetes.io/pciBusID` always identifies the allocated device
itself and retains the inventory's spelling for RSD matching; only sysfs
lookup paths normalize the address to lowercase. Product, NUMA, PCIe-root
and memory information are retained; memory
capacity is the value reported for that VF, not a fabricated division of PF
memory. `matchAttribute: npu.rebellions.ai/parentPCIBusID` can keep multiple
VF requests on the same PF. VFIO devices expose the same `vf`, `partitionMode`,
`numVFs` and `parentPCIBusID` attributes while keeping `type: vfio`.
The existing `vfio-npu.rebellions.ai` class selects PFs only. Select
`vfio-npu-vf1.rebellions.ai` or `vfio-npu-vf4.rebellions.ai` for a VF;
no additional partition selector is needed.
See the [VM passthrough example](examples/kubevirt-vm-passthrough.md).

VF logical names include their profile: `rbln0-0-vf4` for a native VF and
`vfio-0000-17-00-1-vf4` for a passthrough VF. Native `deviceNodeName` still
identifies the actual kernel node (`rbln0-0` in this example). PF names stay
unchanged. VF4 to VF1 changes the logical identity even when the BDF is reused,
so an old unprepared allocation cannot silently become a different profile.

Passthrough currently requires an IOMMU group containing exactly the allocated
PCI function. Missing or shared groups are excluded; the driver does not
advertise members of a shared group as independently allocatable devices.

### Partition lifecycle

The plugin rescans native and vfio-pci devices every `DEVICE_SCAN_INTERVAL`
(`--device-scan-interval`, default `5s`; zero disables the loop) and
republishes the ResourceSlice only when the inventory changes. The legacy
`--vfio-rescan-interval` and `VFIO_RESCAN_INTERVAL` names remain accepted and
now control the shared scan loop. If both environment variables are set,
`DEVICE_SCAN_INTERVAL` takes precedence. New VFs and
devices that recover from an indeterminate SR-IOV state appear after a
successful scan. A scan that lands while device-config-manager is still creating
VFs may publish a partial set; later scans converge. `rbln-smi` runs only when
sysfs shows an NPU bound to the native driver, so VM-passthrough nodes never
execute it.

If a sysfs inventory read or `rbln-smi` query fails, discard that scan and keep
the last complete inventory. Old native devices are never merged with newly
scanned VFIO devices. The next successful scan replaces both lists together;
a successful empty scan removes devices. Required per-device read/parse errors
also discard the scan. Confirmed unsupported profiles, unbound functions and
removed PCI entries are excluded; unavailable optional topology is omitted.
Unreadable vendor IDs on unknown PCI entries are ignored. For devices or
parent PFs already in the published inventory, a vendor read failure preserves
the last snapshot instead of removing those devices.
At startup, an unavailable
`rbln-smi` leaves native inventory empty until a scan succeeds. With periodic
scanning disabled, discovery recovery requires a plugin restart.

Prepared claims and their CDI files are independent of scanning. Common runtime
CDI edits are created when preparing a new native claim, so a toolkit that
appears after plugin startup can be used without a restart. If those edits
cannot be read or written, Prepare returns an error for kubelet to retry.

Provision VFs before starting DRA. Before changing partitions, drain NPU
consumers and wait for them to terminate and release their allocations, then
wait for the new inventory before admitting new workloads. Do not reuse
pre-change ResourceClaims across repartitioning. The device-config-manager owns
VF creation/removal, driver binding, drain and plugin refresh; scanning does
not make live repartitioning safe. For managed provisioning and readiness,
see the [Operator documentation](https://github.com/RBLN-SW/rbln-npu-operator/blob/main/README.md)
and [device-config-manager documentation](https://github.com/RBLN-SW/k8s-device-config-manager/blob/main/README.md).
Standalone Helm installations must enforce this sequence themselves. Do not
expose the same NPUs through both device-plugin and DRA on a node.

See [a Pod requesting one VF](examples/single-pod-npu-vf4.md) for a runnable
ResourceClaimTemplate example. VFIO claims use the existing passthrough CDI
path and do not require the native runtime or an RSD group.

VFIO metadata is stored on the host under
`/var/run/kubernetes.io/dra-device-attributes/resourceclaims/.uid-<claimUID>/`.
Each request's file is bind-mounted read-only at the existing name/alias-based
container paths. Multiple VMs can reuse the same template and pod claim alias,
and direct claims in different namespaces can share a name. DeviceClass names,
device names, request names and the metadata JSON schema are unchanged.

Already prepared claims retain their checkpointed CDI mounts and legacy files
until Unprepare. The checkpoint remains V1. Owner markers and directory depth
are preserved so both current and older cleanup code can remove UID-scoped
files. Downgrading still restores the older collision behavior for newly
prepared claims; host-side scripts that inspect metadata should use the CDI
mount source rather than assume a claim-name directory.

The checkpoint format remains V1. RSD creation uses `rblnlib-go` v0.3.0.
For all native NPU claims, including PFs and VFs, an RSD creation error or an
empty returned device path fails Prepare and leaves the Pod waiting for
successful preparation. Because `rblnlib-go` v0.3.0 can omit unmatched PCI bus IDs without returning
an error, the driver verifies `rbln-smi -g -j` after creation. The returned
private group must contain exactly the requested PCI addresses; partial,
extra, duplicate or shared-group membership fails Prepare before CDI injection.
RSD members are ordered by PCI bus ID for stable group-local NPU numbering.

DeviceClass selectors remain the scheduler's responsibility. Before creating
new RSD/CDI state, Prepare verifies the allocated device's live BDF, vendor,
driver binding and partition layout; VFIO also verifies the IOMMU group and
its isolation. A failed check returns an error for kubelet to retry. Repeated
Prepare calls reuse the checkpointed result. Claim migration and reallocation
across partition changes are outside discovery's scope.

The operator creates and cleans up the same six classes as Helm. For development
clusters that used earlier experimental SR-IOV builds, recreate test consumers
and their claims if they reference the old unsuffixed VF names. There is no
released SR-IOV allocation contract to migrate.

Compatibility notes: these checks also apply to physical NPU workloads.
RSD failures no longer allow a degraded Pod to start, and a missing PCI sysfs
entry is excluded rather than advertised without PCIe topology information.
Validate PF workloads as well as VF1/VF4 before rolling out the new build.

Manual hardware checks for an already provisioned, dedicated node:

```bash
KUBECONFIG=/path/to/test/kubeconfig SRIOV_NODE=<node> MODE=vf4 \
  test/e2e/run-sriov.sh
```

Run for `none`, `vf1`, and `vf4` after device-config-manager converges each layout.
The script is intentionally outside the general CI cases because it requires
a preconfigured SR-IOV node. It checks distinct allocations, VF PCI addresses,
non-default RSD groups, CDI injection, and Pod reuse of a claim. It does not
change partitions or restart the plugin.

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

JSON records use the field names kubelet's own JSON logs use, with `ts`
encoded the way the other Rebellions components encode it, so one collector
configuration can index them all into one field mapping:

| Key | Type | Notes |
|---|---|---|
| `ts` | string | RFC3339Nano, in both formats. |
| `level` | string | `error`, `warn`, `info`, `debug`, `trace` — a closed vocabulary. |
| `v` | number | klog verbosity depth, present on `debug` and `trace` records only. `debug` is `4`, `trace` up to `8`. |
| `msg` | string | |
| `err` | string | Present on failures. |
| `caller` | string | `dir/file.go:line`, added at `debug` and below only. |
| `logger` | string | `glog` on records relayed from `rblnlib-go` (RSD group create/destroy), `grpc` on grpc-go's own records (INFO lands at `debug`), `stderr` on anything else the process wrote to stderr. Absent on the driver's own records. |
| `impact` | string | On the warn/error records where the driver degrades a workload instead of failing it, states what breaks. |

Because `level` buckets every klog depth into five names, `v` is what
distinguishes a `V(5)` record from a `V(7)` one — filter on it to make `trace`
readable, e.g. `jq 'select(.v <= 5)'`.

`rblnlib-go` logs through glog, which has no sink API. The plugin captures its
stderr and re-emits each line as a record at glog's own severity, so RSD-group
lines arrive as JSON rather than as glog text between the JSON records.

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
| VMs stay `Pending`, no passthrough device offered | `Failed to enumerate vfio-pci NPU devices` or `Skipping NPU not usable in its current PCI state` (error, with the reason); a passthrough-only node reports `No NPU bound to the native driver, skipping rbln-smi` instead of running it |
| Devices came, went or changed profile without a restart | `Device inventory changed, republishing resources` with `added` / `removed` / `changed`; an excluded native or VFIO device is reported at startup and when it leaves the inventory, without repeating the exclusion every tick |
| Scan loop may be failing or too slow | `Device scan failed, keeping last inventory` or `NPU enumeration failed, keeping last inventory` (error, once per failing streak, with a matching `recovered` info); both lists stay at the last complete snapshot. `Device scan slower than its interval` (warn); at `debug`, each successful scan records counts, `skipped` reasons and `durationMs` |
| Pod stuck in `ContainerCreating` | `Failed to prepare devices for claim` (error) with `err` and the claim keys |
| Multi-NPU job's RSD group could not be created | `Failed to prepare devices for claim` (error) with an `err` naming the RSD group; RSD creation is mandatory, so the pod stays in `ContainerCreating` instead of starting degraded |
| Container cannot see `/dev/rsd0` or the NPU | `Wrote CDI spec for claim` at `debug`, which lists the injected `deviceNodes` |
| Container cannot find the userspace library | `Wrote common CDI spec with runtime edits` at `debug`, with `mounts`/`hooks` counts |
| Devices seem leaked after a pod is gone | `Unprepared devices for claim`, or `No prepared state for claim` at `debug` |
| Topology-aware allocation behaves oddly | `Ignoring unparseable NUMA node` (warn), `Device reports no NUMA affinity` at `debug` |
| Cross-driver `matchAttribute` on `pcieRoot` never matches | `Ignoring unresolvable PCIe root` (warn); the attribute is resolved from sysfs, so check `/sys` is visible to the plugin |
| A VM's claim fails with "metadata directory ... is owned by claim" | Inspect the owner marker for unexpected or stale state; ordinary name/alias reuse is isolated by claim UID. `Failed to roll back KubeVirt device metadata` reports unsuccessful cleanup. |

## Usage Examples

See `examples/README.md` for usage examples.

## PR CI

Obedients loads the [RNPM PR bootstrap](.buildkite/pipeline.rnpm.pr.yml).
The registered RNPM `pr` bundle defines the image build and Sentinel tests;
this repository no longer keeps a separate legacy Buildkite PR pipeline.

## RC validation

Release candidates are tested through RNPM using their published images.
See [RC validation setup and image inputs](docs/rc-validation.md).

The gRPC health service exposes `readiness` in addition to `liveness`. Readiness
requires kubelet registration acknowledgement as well as working plugin RPCs.
It does not assert that asynchronous ResourceSlice publication has completed;
device-config-manager checks the published inventory before releasing its cordon.
Use a matching image when enabling the chart/operator readinessProbe.
