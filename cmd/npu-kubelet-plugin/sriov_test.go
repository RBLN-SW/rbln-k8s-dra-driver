package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	rblndevice "github.com/rbln-sw/rblnlib-go/pkg/device"
	resourceapi "k8s.io/api/resource/v1"
)

func TestNpuInventorySRIOVMixedNode(t *testing.T) {
	buf := logtest.Capture(t, "info")
	root := t.TempDir()
	var inventory []rblndevice.Device
	for i, count := range []int{0, 1, 4, 2} {
		pf := fmt.Sprintf("0000:%02x:00.0", i+1)
		writeSRIOVAttr(t, root, pf, fmt.Sprint(count))
		inventory = append(inventory, rblndevice.Device{Name: fmt.Sprintf("rbln%d", i), PCIBusID: pf})
		for j := 0; j < count; j++ {
			vf := fmt.Sprintf("0000:%02x:00.%d", i+1, j+1)
			linkSRIOVVF(t, root, vf, pf)
			inventory = append(inventory, rblndevice.Device{
				Name: fmt.Sprintf("rbln%d-%d", i, j), PCIBusID: vf,
				ProductName: "RBLN-CR13", MemoryTotalBytes: 16 << 30, PCINumaNode: "0",
			})
		}
	}
	// A stale SMI entry must not silently become a full PF.
	inventory = append(inventory, rblndevice.Device{Name: "stale", PCIBusID: "0000:ff:00.0"})
	devices, skipped, err := npuDevicesFromInventory(context.Background(), root, inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 6 {
		t.Fatalf("got %d devices, want one PF + five supported VFs: %v", len(devices), devices)
	}
	// Enumeration runs every scan tick, so it must not report skips itself;
	// callers decide when a skip is news.
	for _, msg := range []string{"Excluding VF-hosting PF from advertisement", "Skipping NPU not usable in its current PCI state"} {
		if logtest.Find(logtest.Lines(t, buf), msg) != nil {
			t.Fatalf("enumeration reported a skip on its own: %s", buf.String())
		}
	}
	skippedByName := map[string]skippedDevice{}
	for _, d := range skipped {
		skippedByName[d.name] = d
	}
	if len(skippedByName) != 6 {
		t.Fatalf("skipped = %v, want two hosting PFs, the unsupported PF with its two VFs, and the stale entry", skipped)
	}
	for _, name := range []string{"rbln1", "rbln2"} {
		if d := skippedByName[name]; d.err != nil || !d.sriov.isHostingPF() {
			t.Errorf("%s must be an expected hosting-PF exclusion, got %+v", name, d)
		}
	}
	for _, name := range []string{"rbln3", "rbln3-0", "rbln3-1", "stale"} {
		if d := skippedByName[name]; d.err == nil {
			t.Errorf("%s must carry the SR-IOV error, got %+v", name, d)
		}
	}
	logSkippedDevices(context.Background(), skipped)
	// Older PF DeviceClasses match only type == "npu". They must not select VFs.
	var legacyPFMatches []string
	for _, d := range devices {
		if deviceStringAttr(d, deviceTypeAttributeKey) == "npu" {
			legacyPFMatches = append(legacyPFMatches, d.Name)
		}
		if d.Name == "rbln0" {
			if deviceBoolAttr(d, vfAttributeKey) != false || deviceStringAttr(d, partitionModeAttributeKey) != "none" {
				t.Errorf("unpartitioned PF attributes: %v", d.Attributes)
			}
			continue
		}
		mode, parent, count := "vf4", "0000:03:00.0", int64(4)
		if d.Name == "rbln1-0-vf1" {
			mode, parent, count = "vf1", "0000:02:00.0", 1
		}
		if deviceStringAttr(d, deviceTypeAttributeKey) != "npu-vf" ||
			deviceBoolAttr(d, vfAttributeKey) != true || deviceStringAttr(d, partitionModeAttributeKey) != mode ||
			deviceStringAttr(d, parentPCIBusIDAttributeKey) != parent || deviceIntAttr(d, numVFsAttributeKey) != count {
			t.Errorf("VF attributes: %v", d.Attributes)
		}
		capacity := d.Capacity["memory"]
		if capacity.Value.Value() != 16<<30 {
			t.Errorf("VF memory = %v", capacity.Value.String())
		}
		if deviceIntAttr(d, numaNodeAttributeKey) != int64(0) {
			t.Errorf("missing VF NUMA: %v", d.Attributes)
		}
	}
	if !reflect.DeepEqual(legacyPFMatches, []string{"rbln0"}) {
		t.Fatalf("legacy PF class selected %v, want only the unpartitioned PF", legacyPFMatches)
	}
	lines := logtest.Lines(t, buf)
	hosting := logtest.Find(lines, "Excluding VF-hosting PF from advertisement")
	if hosting == nil || hosting["level"] != "info" {
		t.Fatalf("hosting PF exclusion must be visible at info level: %s", buf.String())
	}
	skippedRecord := logtest.Find(lines, "Skipping NPU not usable in its current PCI state")
	if skippedRecord == nil || skippedRecord["impact"] == nil || skippedRecord["level"] != "error" || skippedRecord["device"] != "rbln3" {
		t.Fatalf("unsupported PF mode must be an error with scheduling impact: %s", buf.String())
	}
	state := &DeviceState{nodeName: "node-1", npuDevices: devices}
	state.rebuildResources()
	if got := state.DriverResources().Pools["node-1"].Slices[0].Devices; !reflect.DeepEqual(got, devices) {
		t.Fatal("published inventory differs from supported compute devices")
	}
}

func TestNpuInventoryPreservesPCIBusIDForRSD(t *testing.T) {
	for _, vf := range []bool{false, true} {
		t.Run(fmt.Sprintf("vf=%t", vf), func(t *testing.T) {
			root := t.TempDir()
			pf := "000a:ab:0c.0"
			bus := pf
			count := "0"
			if vf {
				count = "4"
				bus = "000a:ab:0c.1"
				linkSRIOVVF(t, root, bus, pf)
			}
			writeSRIOVAttr(t, root, pf, count)
			reportedBusID := strings.ToUpper(bus)
			devices, _, err := npuDevicesFromInventory(context.Background(), root, []rblndevice.Device{{
				Name: "null", PCIBusID: reportedBusID, PCINumaNode: "0",
			}})
			if err != nil {
				t.Fatal(err)
			}
			if len(devices) != 1 || deviceStringAttr(devices[0], pciBusIDAttributeKey) != reportedBusID {
				t.Fatalf("PCI lookup must accept uppercase IDs without changing the published address: %v", devices)
			}
			st := prepareStateFor(t, "/dev/null")
			st.sysfsPCIDevicesRoot = root
			st.allocatable = AllocatableDevices{devices[0].Name: devices[0]}
			st.rsdGroupFn = func(ids []string) (string, error) {
				if !reflect.DeepEqual(ids, []string{reportedBusID}) {
					t.Fatalf("RSD must receive the original inventory address, got %v", ids)
				}
				return "/dev/null", nil
			}
			claim := allocatedClaim()
			claim.Status.Allocation.Devices.Results[0].Device = devices[0].Name
			if _, err := st.Prepare(context.Background(), claim); err != nil {
				t.Fatalf("prepare with uppercase PCI address: %v", err)
			}
		})
	}
}

func TestPrepareSRIOVUsesVFAddressesAndNodes(t *testing.T) {
	st := prepareStateFor(t, "/dev/null")
	pf := "0000:17:00.0"
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), nativeVF("0000:17:00.1", pf), nativeVF("0000:17:00.2", pf)})
	st.inventoryFn = inventoryOf(npuEntry("null", "0000:17:00.1"), npuEntry("zero", "0000:17:00.2"))
	rescan(t, st)
	var addresses []string
	st.rsdGroupFn = func(ids []string) (string, error) {
		addresses = ids
		return "/dev/null", nil
	}
	claim := allocatedClaim()
	claim.Status.Allocation.Devices.Results[0].Device = "null-vf4"
	second := claim.Status.Allocation.Devices.Results[0]
	second.Device = "zero-vf4"
	// Deliberately reverse allocation order to exercise stable RSD member ordering.
	claim.Status.Allocation.Devices.Results = append([]resourceapi.DeviceRequestAllocationResult{second}, claim.Status.Allocation.Devices.Results...)
	prepared, err := st.Prepare(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(addresses, []string{"0000:17:00.1", "0000:17:00.2"}) {
		t.Fatalf("group addresses = %v", addresses)
	}
	if len(prepared) != 2 {
		t.Fatalf("prepared = %v", prepared)
	}
	for _, d := range prepared {
		if len(d.CdiDeviceIds) != 2 {
			t.Errorf("VF must receive runtime + claim CDI IDs: %v", d.CdiDeviceIds)
		}
	}
	checkpoint := newCheckpoint()
	if err := st.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		t.Fatal(err)
	}
	for _, d := range checkpoint.V1.PreparedClaims[string(claim.UID)] {
		nodes := d.ContainerEdits.DeviceNodes
		if len(nodes) != 2 || nodes[0].Path != "/dev/rsd0" || nodes[1].HostPath != "/dev/"+strings.TrimSuffix(d.DeviceName, "-vf4") {
			t.Fatalf("unexpected VF nodes: %+v", nodes)
		}
	}
	if err := st.Unprepare(context.Background(), string(claim.UID)); err != nil {
		t.Fatal(err)
	}
}
