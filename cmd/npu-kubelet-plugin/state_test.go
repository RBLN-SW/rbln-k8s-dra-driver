package main

import (
	"context"
	"errors"
	"os"
	"strings"
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
	st := newTestState(t)
	st.nodeName = "node-1"
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:4d:00.0", "0")})
	st.cdi = cdiHandlerWithRuntimeSpec(t)
	st.rsdGroupFn = func([]string) (string, error) { return rsdPath, nil }
	st.allocatable = AllocatableDevices{
		"null": {
			Name: "null",
			Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
				pciBusIDAttributeKey:       {StringValue: ptr.To("0000:4d:00.0")},
				deviceTypeAttributeKey:     {StringValue: ptr.To(deviceTypeNpu)},
				deviceNodeNameAttributeKey: {StringValue: ptr.To("null")},
			},
		},
	}
	(sriovInfo{}).setAttributes(st.allocatable["null"].Attributes)
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

// Preparation must fail when its RSD group cannot be created.
func TestPrepareRejectsFailedRSDGroup(t *testing.T) {
	for _, deviceType := range []string{"npu", "npu-vf"} {
		t.Run(deviceType, func(t *testing.T) {
			for _, groupErr := range []error{nil, errors.New("group creation failed")} {
				st := prepareStateFor(t, "")
				st.allocatable["null"].Attributes[deviceTypeAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(deviceType)}
				st.rsdGroupFn = func([]string) (string, error) { return "", groupErr }
				devices, err := st.Prepare(context.Background(), allocatedClaim())
				if err == nil || !strings.Contains(err.Error(), "RSD group") || len(devices) != 0 {
					t.Fatalf("Prepare = %v, %v; want no devices and RSD error", devices, err)
				}
				if groupErr != nil && !errors.Is(err, groupErr) {
					t.Fatalf("RSD error lost: %v", err)
				}
				files, err := os.ReadDir(st.cdi.root)
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range files {
					if strings.Contains(file.Name(), string(allocatedClaim().UID)) {
						t.Fatalf("failed preparation wrote claim CDI file: %s", file.Name())
					}
				}
				cp := newCheckpoint()
				if err := st.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, cp); err != nil {
					t.Fatal(err)
				}
				if len(cp.V1.PreparedClaims) != 0 {
					t.Fatal("failed preparation must not be checkpointed as prepared")
				}
			}
		})
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

	added, removed, changed := deviceNameDelta(prev, next)
	if len(added) != 1 || added[0] != "c" {
		t.Errorf("added = %v, want [c]", added)
	}
	if len(removed) != 1 || removed[0] != "a" {
		t.Errorf("removed = %v, want [a]", removed)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want none", changed)
	}

	// Attribute updates on an unchanged identity do not appear in added/removed.
	typed := func(name, deviceType string) resourceapi.Device {
		return resourceapi.Device{Name: name, Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {StringValue: ptr.To(deviceType)},
		}}
	}
	_, _, changed = deviceNameDelta([]resourceapi.Device{typed("a", "npu"), dev("b")}, []resourceapi.Device{typed("a", "npu-vf"), dev("b")})
	if len(changed) != 1 || changed[0] != "a" {
		t.Errorf("changed = %v, want [a]", changed)
	}

	// Empty, not nil: the record should read "added":[] rather than null.
	added, removed, changed = deviceNameDelta(nil, nil)
	if added == nil || removed == nil || changed == nil || len(added) != 0 || len(removed) != 0 || len(changed) != 0 {
		t.Errorf("delta of nothing = %v/%v/%v, want three empty slices", added, removed, changed)
	}
}

// rbln-smi prints numa_node "N/A" where sysfs reports -1. Both mean the device
// has no NUMA affinity, so "N/A" is the debug omission, not a warn about
// malformed input that fires once per device on every start.
func TestSetNumaNodeAttrTreatsNAAsNoAffinity(t *testing.T) {
	buf := logtest.Capture(t, "debug")

	attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{}
	setNumaNodeAttr(context.Background(), attrs, "rbln0", "N/A")

	if len(attrs) != 0 {
		t.Errorf("attrs = %v, want none for numa_node=N/A", attrs)
	}
	lines := logtest.Lines(t, buf)
	if logtest.Find(lines, "Ignoring unparseable NUMA node") != nil {
		t.Error("N/A must not be reported as unparseable")
	}
	line := logtest.Find(lines, "Device reports no NUMA affinity, omitting numaNode attribute")
	if line == nil {
		t.Fatalf("no-affinity omission was not reported at debug: %s", buf.String())
	}
	if line["device"] != "rbln0" || line["numaNode"] != "N/A" {
		t.Errorf("device/numaNode = %v/%v", line["device"], line["numaNode"])
	}
}
