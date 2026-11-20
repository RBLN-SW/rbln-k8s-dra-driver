# RSD request views for native DRA claims

A claim can allocate two NPUs through named requests. A regular init container can consume the whole claim and the app can select just one request. Both containers open `/dev/rsd0`. For the app to receive a one-device view, the driver group behind that endpoint must contain exactly the selected PCI function.

## Using the draft implementation

Set `npu.rebellions.ai/rsd-request-view: "v1"` in the ResourceClaim's `metadata.annotations` before Kubernetes prepares it. For a ResourceClaimTemplate, set it in `spec.metadata.annotations` on the generated claim template. The experimental path supports a native claim and an OCI runtime with CDI `createRuntime` hooks. With the standalone Helm chart, enable `kubeletPlugin.installRsdRequestViewHook`. On Operator-managed nodes, provision the executable from the matching DRA image using `/usr/bin/rsd-claim-handoff install` with only the host directory `/opt/rbln/oci-hooks/v1` mounted there. The node needs the Rebellions SMI. The claim journal is stored separately under `/run/rbln`. Keep the hook available for the lifetime of claims prepared with this mode.

The kubelet plugin records the claim UID, PCI functions, private RSD slot, Linux device number and node boot ID at prepare. Each requested CDI entry contributes a hook. At container creation, the hook reads the selected entries from the OCI bundle and serializes access to the node's RSD writer lock. It checks the effective membership and the device number before allowing the process to start. A root-owned per-claim file records pending transitions and active OCI process identities on the node.

| If two containers share a claim | Behavior |
| --- | --- |
| P requests both A and B, then Q requests both | Q can start on the same group. |
| P is running with A and B, then Q requests only B | Q waits. P keeps both members. After P exits, Q can retry with B. |
| P is running with only B, then Q requests A and B | Q waits. P retains B until it exits. |
| The hook or its state is unavailable | The runtime fails container creation. Recovery depends on the runtime and Pod restart policy. A terminal StartError in a Never Pod requires Pod recreation. |

A reservation on the claim can remain after a particular container exits. Leases therefore identify the runtime process by container ID, PID, process start time and host boot ID. Group transitions use a write-ahead record, an SMI membership check and the device number already selected by the runtime. The next start can reconcile an interrupted transition within that claim. Unexpected ownership or device-number changes require recovery before a container can start. The hook waits up to 75 seconds for an incompatible live lease, subject to the runtime deadline. On timeout, the existing group is preserved and the runtime reports the start failure. Node reboot and driver reload require separate validation.

## What independent views need from the driver and runtime

Consider P using `{A,B}` and Q using `{B}` at the same time. Both processes must continue running while each observes its own set. Today's single-membership group operation can sequence those consumers. Supporting them concurrently requires an independent view of the allocation for each consumer.

| Layer | Required behavior |
| --- | --- |
| Kernel driver | Create a handle or character endpoint bound to a specific set of PCI functions and an allocation generation. Opening Q's view or releasing P's view preserves all other live handles and their membership. Enforce membership on enumeration, submission and memory operations. |
| User-mode driver and SMI | Enumerate and operate through the view opened as `/dev/rsd0`. Device numbering within that view must remain stable for its lifetime. Report the effective PCI identities for diagnostics. |
| Device Plugin and DRA | Persist enough identity to resolve an existing allocation after a plugin restart. Bind the per-container CDI selection to a view, allow compatible sharing, and let the kernel track live references. Define compatibility for checkpointed legacy direct paths. |
| Recovery and acceptance | Keep handles valid until the final user releases them. Define reload, reboot and hot-removal behavior. Exercise both arrival orders for overlapping sets, a disjoint peer, restarts and failures. Run actual SDK workloads concurrently and verify that releasing either consumer preserves the other's results. |

The design still needs an ABI decision on whether a view is identified by an independent device number or by a per-open handle. A host-path alias alone cannot give two live consumers different membership in one mutable group.
