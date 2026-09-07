package main

import (
	"context"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/consts"
)

// prepareStateFor builds a DeviceState able to run prepareDevices end to end
// without NPU hardware. The device is named "null" so the per-device node path
// resolves to /dev/null, which already exists — otherwise applyConfig would
// block on the device-node poll.
func prepareStateFor(t *testing.T, rsdPath string) *DeviceState {
	t.Helper()
	cdi, err := NewCDIHandler(t.TempDir(), consts.DriverName, "npu")
	if err != nil {
		t.Fatalf("CDI handler: %v", err)
	}
	st := newTestState(t)
	st.cdi = cdi
	st.rsdGroupFn = func([]string) string { return rsdPath }
	st.allocatable = AllocatableDevices{
		"null": {
			Name: "null",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				pciBusIDAttributeKey: {StringValue: ptr.To("0000:4d:00.0")},
			},
		},
	}
	return st
}

func allocatedClaim() *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "npu-claim", UID: "claim-uid-3"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Driver:  consts.DriverName,
						Device:  "null",
						Request: "npu",
						Pool:    "node-1",
					}},
				},
			},
		},
	}
}

// An empty RSD group path makes applyConfig skip /dev/rsd0 entirely: the
// container starts, the pod goes Ready, and multi-NPU peer communication is
// broken with nothing in the log to say so. This is the component's core
// invariant failing silently, so it has to be an error record.
func TestPrepareDevicesLogsMissingRSDGroupAsError(t *testing.T) {
	buf := logtest.Capture(t, "info")
	st := prepareStateFor(t, "")

	if _, err := st.prepareDevices(context.Background(), allocatedClaim()); err != nil {
		t.Fatalf("prepareDevices: %v", err)
	}

	line := logtest.Find(logtest.Lines(t, buf), "RSD group creation returned no device path")
	if line == nil {
		t.Fatalf("missing RSD group was not reported: %s", buf.String())
	}
	if line["level"] != "error" {
		t.Errorf("level = %v, want error", line["level"])
	}
	if line["busIDs"] == nil {
		t.Errorf("busIDs missing: the operator needs to know which devices were grouped")
	}
	// The message alone reads like a transient failure; the record has to say
	// the pod is Ready but degraded, because nothing else will.
	if line["impact"] == nil {
		t.Errorf("impact missing: the record must say what breaks for the workload")
	}
}

// The success path must be visible too: /dev/rsd0 is the thing operators check
// first when peer communication misbehaves.
func TestPrepareDevicesLogsCreatedRSDGroup(t *testing.T) {
	buf := logtest.Capture(t, "info")
	st := prepareStateFor(t, "/dev/null")

	if _, err := st.prepareDevices(context.Background(), allocatedClaim()); err != nil {
		t.Fatalf("prepareDevices: %v", err)
	}

	line := logtest.Find(logtest.Lines(t, buf), "Created RSD group device")
	if line == nil {
		t.Fatalf("RSD group creation was not logged: %s", buf.String())
	}
	if line["hostRsdPath"] != "/dev/null" {
		t.Errorf("hostRsdPath = %v, want /dev/null", line["hostRsdPath"])
	}
}

// Prepare names the devices it injected; unprepare has to name the same ones,
// or an operator cannot pair the two halves of a claim's lifecycle and tell
// whether a device node was actually released.
func TestUnprepareLogsTheDevicesItReleased(t *testing.T) {
	buf := logtest.Capture(t, "info")
	st := prepareStateFor(t, "/dev/null")
	claim := allocatedClaim()

	if _, err := st.Prepare(context.Background(), claim); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := st.Unprepare(context.Background(), string(claim.UID)); err != nil {
		t.Fatalf("Unprepare: %v", err)
	}

	line := logtest.Find(logtest.Lines(t, buf), "Unprepared devices for claim")
	if line == nil {
		t.Fatalf("unprepare was not logged: %s", buf.String())
	}
	devices, ok := line["devices"].([]any)
	if !ok || len(devices) != 1 || devices[0] != "null" {
		t.Errorf("devices = %#v, want [null]", line["devices"])
	}
}

// A malformed NUMA value silently dropped the attribute, quietly degrading
// topology-aware allocation.
func TestEnumerationWarnsOnUnparseableNumaNode(t *testing.T) {
	buf := logtest.Capture(t, "info")

	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
	setNumaNodeAttr(context.Background(), attrs, "rbln0", "not-a-number")

	if _, ok := attrs[numaNodeAttributeKey]; ok {
		t.Error("numaNode attribute must not be set from an unparseable value")
	}
	line := logtest.Find(logtest.Lines(t, buf), "Ignoring unparseable NUMA node")
	if line == nil {
		t.Fatalf("unparseable NUMA node was not reported: %s", buf.String())
	}
	if line["device"] != "rbln0" || line["numaNode"] != "not-a-number" {
		t.Errorf("device/numaNode = %v/%v", line["device"], line["numaNode"])
	}
}

// sysfs reports numa_node=-1 for a device with no NUMA affinity. Publishing
// -1 would make the attribute match nothing, so it must be omitted; a real
// node is mirrored under the DraNet key so one claim can matchAttribute NPUs
// against NICs and CPUs published by other drivers.
func TestSetNumaNodeAttrOmitsNegativeAndMirrorsDraNetKey(t *testing.T) {
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}

	setNumaNodeAttr(context.Background(), attrs, "rbln0", "-1")
	if len(attrs) != 0 {
		t.Errorf("attrs = %v, want none for numa_node=-1", attrs)
	}

	setNumaNodeAttr(context.Background(), attrs, "rbln0", "1")
	for _, key := range []resourceapi.QualifiedName{numaNodeAttributeKey, dranetNumaNodeAttributeKey} {
		attr, ok := attrs[key]
		if !ok || attr.IntValue == nil || *attr.IntValue != 1 {
			t.Errorf("%s = %v, want IntValue 1", key, attr.IntValue)
		}
	}
}

// A pcieRoot that cannot be resolved from sysfs silently dropped the
// attribute, and cross-driver matchAttribute against DraNet NICs then never
// matched with nothing in the log to explain why.
func TestSetPCIERootAttrWarnsWhenUnresolvable(t *testing.T) {
	buf := logtest.Capture(t, "info")
	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}

	if got := setPCIERootAttr(context.Background(), attrs, "rbln0", t.TempDir(), "0000:4d:00.0"); got != "" {
		t.Errorf("pcieRoot = %q, want empty for a device missing from sysfs", got)
	}
	if _, ok := attrs[pcieRootAttributeKey]; ok {
		t.Error("pcieRoot attribute must not be set when it cannot be resolved")
	}

	line := logtest.Find(logtest.Lines(t, buf), "Ignoring unresolvable PCIe root")
	if line == nil {
		t.Fatalf("unresolvable PCIe root was not reported: %s", buf.String())
	}
	if line["level"] != "warn" {
		t.Errorf("level = %v, want warn", line["level"])
	}
	if line["device"] != "rbln0" || line["pciBusID"] != "0000:4d:00.0" {
		t.Errorf("device/pciBusID = %v/%v", line["device"], line["pciBusID"])
	}
	if line["impact"] == nil {
		t.Error("impact missing: the record must say the device lost its pcieRoot attribute")
	}
}

func TestDeviceNameDelta(t *testing.T) {
	dev := func(name string) resourceapi.Device { return resourceapi.Device{Name: name} }
	prev := []resourceapi.Device{dev("a"), dev("b")}
	next := []resourceapi.Device{dev("b"), dev("c")}

	added, removed := deviceNameDelta(prev, next)
	if len(added) != 1 || added[0] != "c" {
		t.Errorf("added = %v, want [c]", added)
	}
	if len(removed) != 1 || removed[0] != "a" {
		t.Errorf("removed = %v, want [a]", removed)
	}

	// Empty, not nil: the record should read "added":[] rather than null.
	added, removed = deviceNameDelta(nil, nil)
	if added == nil || removed == nil || len(added) != 0 || len(removed) != 0 {
		t.Errorf("delta of nothing = %v/%v, want two empty slices", added, removed)
	}
}
