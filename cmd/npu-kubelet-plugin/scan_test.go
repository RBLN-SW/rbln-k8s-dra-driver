package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	rblndevice "github.com/rbln-sw/rblnlib-go/pkg/device"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

// nativeDevice is a Rebellions NPU bound to the kernel driver; numVFs is the
// sriov_numvfs content, empty for hardware without SR-IOV attributes.
func nativeDevice(busID, numVFs string) fakePCIDevice {
	d := rblnVfioDevice(busID)
	d.driver = "rebellions"
	d.numVFs = numVFs
	return d
}

func nativeVF(busID, parent string) fakePCIDevice {
	d := nativeDevice(busID, "")
	d.physfnOf = parent
	return d
}

func vfioVF(busID, parent string) fakePCIDevice {
	d := rblnVfioDevice(busID)
	d.physfnOf = parent
	return d
}

func npuEntry(name, busID string) rblndevice.Device {
	return rblndevice.Device{Name: name, PCIBusID: busID, ProductName: "RBLN-CR13", MemoryTotalBytes: 16 << 30, PCINumaNode: "0"}
}

func inventoryOf(devs ...rblndevice.Device) func(context.Context) ([]rblndevice.Device, error) {
	return func(context.Context) ([]rblndevice.Device, error) { return devs, nil }
}

func scanState(root string, inventory func(context.Context) ([]rblndevice.Device, error)) *DeviceState {
	st := &DeviceState{nodeName: "node-1", sysfsPCIDevicesRoot: root, inventoryFn: inventory}
	st.rebuildResources()
	return st
}

func publishedNames(resources resourceslice.DriverResources) []string {
	var names []string
	for _, d := range resources.Pools["node-1"].Slices[0].Devices {
		names = append(names, d.Name)
	}
	return names
}

func sameNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A VF-passthrough node keeps its hosting PF on the native driver, so the
// gate, not rbln-smi, has to decide that there is nothing native to publish.
// The startup NPU entry stands in for a node converted after the plugin
// started: one scan must drop it rather than advertise it next to the VFs.
func TestRescanDevicesSkipsRblnSmiOnPassthroughNode(t *testing.T) {
	root := writeFakeSysfs(t, []fakePCIDevice{
		nativeDevice("0000:17:00.0", "4"), vfioVF("0000:17:00.1", "0000:17:00.0"), vfioVF("0000:17:00.2", "0000:17:00.0"),
	})
	calls := 0
	st := scanState(root, func(context.Context) ([]rblndevice.Device, error) {
		calls++
		return nil, errors.New("rbln-smi is not installed on this node")
	})
	st.npuDevices = []resourceapi.Device{{Name: "rbln0", Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeNpu)},
	}}}
	st.rebuildResources()

	resources, changed, err := st.RescanDevices(context.Background())
	if err != nil || !changed {
		t.Fatalf("rescan: changed=%v err=%v", changed, err)
	}
	if calls != 0 {
		t.Fatalf("rbln-smi ran %d times on a VF-passthrough node", calls)
	}
	if got := publishedNames(resources); !sameNames(got, []string{"vfio-0000-17-00-1-vf4", "vfio-0000-17-00-2-vf4"}) {
		t.Fatalf("published %v, want only the two passthrough VFs", got)
	}
}

// The gate decides whether rbln-smi has anything to report. On VM-passthrough
// nodes a VF-hosting PF remains on the native driver while its VFs bind
// to vfio-pci, so a hosting PF alone must not count.
func TestCountNativeCandidates(t *testing.T) {
	other := fakePCIDevice{busID: "0000:65:00.0", vendor: "0x10de", class: npuPCIClassID, driver: "nvidia", rootPort: "0000:00:02.0"}
	unbound := nativeDevice("0000:66:00.0", "0")
	unbound.driver = ""
	for _, tc := range []struct {
		name    string
		devices []fakePCIDevice
		want    int
	}{
		{"PF passthrough", []fakePCIDevice{rblnVfioDevice("0000:17:00.0")}, 0},
		{"VF passthrough", []fakePCIDevice{nativeDevice("0000:17:00.0", "4"), vfioVF("0000:17:00.1", "0000:17:00.0"), vfioVF("0000:17:00.2", "0000:17:00.0")}, 0},
		{"PF container", []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}, 1},
		{"legacy PF without SR-IOV attributes", []fakePCIDevice{nativeDevice("0000:17:00.0", "")}, 1},
		{"VF container", []fakePCIDevice{nativeDevice("0000:17:00.0", "4"), nativeVF("0000:17:00.1", "0000:17:00.0"), nativeVF("0000:17:00.2", "0000:17:00.0"), nativeVF("0000:17:00.3", "0000:17:00.0"), nativeVF("0000:17:00.4", "0000:17:00.0")}, 4},
		{"mixed PF modes", []fakePCIDevice{nativeDevice("0000:17:00.0", "0"), nativeDevice("0000:65:00.0", "1"), nativeVF("0000:65:00.1", "0000:65:00.0")}, 2},
		{"other vendor and unbound devices", []fakePCIDevice{other, unbound}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeFakeSysfs(t, tc.devices)
			got, err := countNativeCandidates(root, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("candidates = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCountNativeCandidatesFailsWhenSysfsUnreadable(t *testing.T) {
	if _, err := countNativeCandidates(filepath.Join(t.TempDir(), "missing"), nil); err == nil {
		t.Fatal("unreadable sysfs must be an error, not zero candidates")
	}
}

func TestRescanDevicesPublishesNativeAndPassthroughTogether(t *testing.T) {
	root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0"), rblnVfioDevice("0000:27:00.0")})
	st := scanState(root, inventoryOf(npuEntry("rbln0", "0000:17:00.0")))

	resources, changed, err := st.RescanDevices(context.Background())
	if err != nil || !changed {
		t.Fatalf("first rescan: changed=%v err=%v", changed, err)
	}
	if got := publishedNames(resources); !sameNames(got, []string{"rbln0", "vfio-0000-27-00-0"}) {
		t.Fatalf("published %v, want the native PF and the passthrough device", got)
	}
	if _, ok := st.allocatable["rbln0"]; !ok {
		t.Error("native device missing from allocatable")
	}

	if _, changed, err = st.RescanDevices(context.Background()); err != nil || changed {
		t.Fatalf("second rescan: changed=%v err=%v", changed, err)
	}
}

// The scan ticker gives no other hint of when a device appeared or went, so
// the change record has to name the delta; an unchanged tick must stay silent.
func TestRescanDevicesLogsDelta(t *testing.T) {
	buf := logtest.Capture(t, "info")
	root := writeFakeSysfs(t, []fakePCIDevice{rblnVfioDevice("0000:27:00.0")})
	st := scanState(root, inventoryOf())

	if _, changed, err := st.RescanDevices(context.Background()); err != nil || !changed {
		t.Fatalf("first rescan: changed=%v err=%v", changed, err)
	}
	line := logtest.Find(logtest.Lines(t, buf), "Device inventory changed, republishing resources")
	if line == nil {
		t.Fatalf("device change was not logged: %s", buf.String())
	}
	if added, ok := line["added"].([]any); !ok || len(added) != 1 || added[0] != "vfio-0000-27-00-0" {
		t.Errorf("added = %#v, want [vfio-0000-27-00-0]", line["added"])
	}
	for _, key := range []string{"removed", "changed"} {
		if list, ok := line[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty list", key, line[key])
		}
	}
	if line["npuDeviceCount"] != float64(0) || line["vfioDeviceCount"] != float64(1) {
		t.Errorf("counts = %v/%v, want 0/1", line["npuDeviceCount"], line["vfioDeviceCount"])
	}

	buf.Reset()
	if _, changed, err := st.RescanDevices(context.Background()); err != nil || changed {
		t.Fatalf("second rescan: changed=%v err=%v", changed, err)
	}
	if buf.Len() != 0 {
		t.Errorf("unchanged rescan produced records: %s", buf.String())
	}
}

func rescan(t *testing.T, st *DeviceState) bool {
	t.Helper()
	_, changed, err := st.RescanDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return changed
}

func vf4Layout(t *testing.T) (string, func(context.Context) ([]rblndevice.Device, error)) {
	t.Helper()
	pf := "0000:17:00.0"
	root := writeFakeSysfs(t, []fakePCIDevice{
		nativeDevice(pf, "4"), nativeVF("0000:17:00.1", pf), nativeVF("0000:17:00.2", pf), nativeVF("0000:17:00.3", pf), nativeVF("0000:17:00.4", pf),
	})
	inventory := inventoryOf(npuEntry("rbln0", pf),
		npuEntry("rbln0-0", "0000:17:00.1"), npuEntry("rbln0-1", "0000:17:00.2"), npuEntry("rbln0-2", "0000:17:00.3"), npuEntry("rbln0-3", "0000:17:00.4"))
	return root, inventory
}

// none -> vf4 without a restart: the PF leaves the slice, its VFs arrive, and
// the hosting-PF exclusion is reported once because it is news on this tick.
func TestRescanDevicesFollowsPartitionChange(t *testing.T) {
	buf := logtest.Capture(t, "info")
	st := scanState(writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}), inventoryOf(npuEntry("rbln0", "0000:17:00.0")))
	rescan(t, st)

	st.sysfsPCIDevicesRoot, st.inventoryFn = vf4Layout(t)
	buf.Reset()
	if !rescan(t, st) {
		t.Fatal("partition change was not detected")
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"rbln0-0-vf4", "rbln0-1-vf4", "rbln0-2-vf4", "rbln0-3-vf4"}) {
		t.Fatalf("published %v, want the four VFs", got)
	}
	if mode := deviceStringAttr(st.allocatable["rbln0-0-vf4"], partitionModeAttributeKey); mode != "vf4" {
		t.Errorf("rbln0-0 partitionMode = %q, want vf4", mode)
	}
	lines := logtest.Lines(t, buf)
	change := logtest.Find(lines, "Device inventory changed, republishing resources")
	if change == nil {
		t.Fatalf("change not logged: %s", buf.String())
	}
	if removed, _ := change["removed"].([]any); len(removed) != 1 || removed[0] != "rbln0" {
		t.Errorf("removed = %#v, want [rbln0]", change["removed"])
	}
	if added, _ := change["added"].([]any); len(added) != 4 {
		t.Errorf("added = %#v, want four VFs", change["added"])
	}
	hosting := logtest.Find(lines, "Excluding VF-hosting PF from advertisement")
	if hosting == nil || hosting["device"] != "rbln0" || hosting["numVFs"] != float64(4) {
		t.Errorf("hosting PF exclusion must be reported when the PF drops out: %s", buf.String())
	}
}

// Reusing the kernel name across profiles must not reuse the DRA identity.
func TestRescanDevicesChangesVFIdentityWithProfile(t *testing.T) {
	buf := logtest.Capture(t, "info")
	root, inventory := vf4Layout(t)
	st := scanState(root, inventory)
	rescan(t, st)

	pf := "0000:17:00.0"
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "1"), nativeVF("0000:17:00.1", pf)})
	st.inventoryFn = inventoryOf(npuEntry("rbln0", pf), npuEntry("rbln0-0", "0000:17:00.1"))
	buf.Reset()
	if !rescan(t, st) {
		t.Fatal("profile change was not detected")
	}
	if mode := deviceStringAttr(st.allocatable["rbln0-0-vf1"], partitionModeAttributeKey); mode != "vf1" {
		t.Errorf("rbln0-0 partitionMode = %q, want vf1", mode)
	}
	change := logtest.Find(logtest.Lines(t, buf), "Device inventory changed, republishing resources")
	if change == nil {
		t.Fatalf("change not logged: %s", buf.String())
	}
	if added, _ := change["added"].([]any); len(added) != 1 || added[0] != "rbln0-0-vf1" {
		t.Errorf("added = %#v, want [rbln0-0-vf1]", change["added"])
	}
	if removed, _ := change["removed"].([]any); len(removed) != 4 {
		t.Errorf("removed = %#v, want all four old VF4 identities", change["removed"])
	}
}

// A device that drops out because its SR-IOV state became unsupported is
// reported with the reason on the tick it leaves, and not again.
func TestRescanDevicesReportsNewlySkippedDeviceOnce(t *testing.T) {
	buf := logtest.Capture(t, "info")
	inventory := inventoryOf(npuEntry("rbln0", "0000:17:00.0"), npuEntry("rbln1", "0000:65:00.0"))
	st := scanState(writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0"), nativeDevice("0000:65:00.0", "0")}), inventory)
	rescan(t, st)

	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0"), nativeDevice("0000:65:00.0", "2")})
	buf.Reset()
	if !rescan(t, st) {
		t.Fatal("dropped device was not detected")
	}
	lines := logtest.Lines(t, buf)
	skip := logtest.Find(lines, "Skipping NPU not usable in its current PCI state")
	if skip == nil || skip["level"] != "error" || skip["device"] != "rbln1" || skip["impact"] == nil {
		t.Fatalf("newly skipped device must be reported with its reason: %s", buf.String())
	}
	if change := logtest.Find(lines, "Device inventory changed, republishing resources"); change == nil {
		t.Fatalf("change not logged: %s", buf.String())
	} else if removed, _ := change["removed"].([]any); len(removed) != 1 || removed[0] != "rbln1" {
		t.Errorf("removed = %#v, want [rbln1]", change["removed"])
	}

	buf.Reset()
	if rescan(t, st) || buf.Len() != 0 {
		t.Errorf("a device that stays skipped must not be reported again: %s", buf.String())
	}
}

func TestRescanDevicesReportsNewlySkippedVfioOnce(t *testing.T) {
	for _, cause := range []string{"unsupported profile", "missing IOMMU group", "shared IOMMU group"} {
		t.Run(cause, func(t *testing.T) {
			buf := logtest.Capture(t, "info")
			pf, vf := "0000:17:00.0", "0000:17:00.1"
			root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), vfioVF(vf, pf)})
			st := scanState(root, inventoryOf())
			rescan(t, st)
			switch cause {
			case "unsupported profile":
				if err := os.WriteFile(filepath.Join(root, pf, "sriov_numvfs"), []byte("2"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing IOMMU group":
				if err := os.Remove(filepath.Join(root, vf, "iommu_group")); err != nil {
					t.Fatal(err)
				}
			case "shared IOMMU group":
				if err := os.Mkdir(filepath.Join(root, vf, "iommu_group", "devices", "0000:ff:00.0"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			buf.Reset()
			if !rescan(t, st) {
				t.Fatal("excluded VF was not removed")
			}
			line := logtest.Find(logtest.Lines(t, buf), "Skipping NPU not usable in its current PCI state")
			if line == nil || line["pciBusID"] != vf || line["err"] == nil {
				t.Fatalf("exclusion not reported: %s", buf.String())
			}
			buf.Reset()
			if rescan(t, st) || buf.Len() != 0 {
				t.Fatalf("persisting exclusion must stay silent: %s", buf.String())
			}
		})
	}
}

func TestNativeFailureStreakEndsWhenNoNativeCandidatesRemain(t *testing.T) {
	buf := logtest.Capture(t, "info")
	failure := errors.New("SMI unavailable")
	st, err := NewDeviceState(context.Background(), &Config{
		flags:               &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot: writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}),
		inventoryFn:         func(context.Context) ([]rblndevice.Device, error) { return nil, failure },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !st.scan.npuFailing {
		t.Fatal("startup failure must start a failure streak")
	}
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{rblnVfioDevice("0000:17:00.0")})
	rescan(t, st)
	if st.scan.npuFailing {
		t.Fatal("zero native candidates must clear the failure streak")
	}
	buf.Reset()
	rescan(t, st)
	if buf.Len() != 0 {
		t.Fatalf("unchanged VFIO-only tick must stay silent: %s", buf.String())
	}
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")})
	if _, changed, err := st.RescanDevices(context.Background()); !errors.Is(err, failure) || changed {
		t.Fatalf("new failure: changed=%v err=%v", changed, err)
	}
	line := logtest.Find(logtest.Lines(t, buf), "NPU enumeration failed, keeping last inventory")
	if line == nil || line["level"] != "error" {
		t.Fatalf("new failure streak was suppressed: %s", buf.String())
	}
	buf.Reset()
	if _, changed, err := st.RescanDevices(context.Background()); !errors.Is(err, failure) || changed {
		t.Fatalf("persisting failure: changed=%v err=%v", changed, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("persisting failure must stay silent: %s", buf.String())
	}
}

func TestNewDeviceStateReportsVfioExclusionAtStartup(t *testing.T) {
	buf := logtest.Capture(t, "info")
	d := rblnVfioDevice("0000:17:00.0")
	d.iommuGroup = ""
	st, err := NewDeviceState(context.Background(), &Config{
		flags:               &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot: writeFakeSysfs(t, []fakePCIDevice{d}), inventoryFn: inventoryOf(),
	})
	if err != nil {
		t.Fatal(err)
	}
	line := logtest.Find(logtest.Lines(t, buf), "Skipping NPU not usable in its current PCI state")
	if line == nil || line["pciBusID"] != d.busID {
		t.Fatalf("startup exclusion not logged: %s", buf.String())
	}
	buf.Reset()
	rescan(t, st)
	if buf.Len() != 0 {
		t.Fatalf("already reported exclusion must stay silent: %s", buf.String())
	}
}

// Keep one complete snapshot on failure. Applying only the new VFIO list
// would publish the rebound BDF as both native and passthrough.
func TestRescanDevicesKeepsWholeInventoryWhileRblnSmiFails(t *testing.T) {
	buf := logtest.Capture(t, "info")
	var fail error
	st := scanState(writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}), func(context.Context) ([]rblndevice.Device, error) {
		if fail != nil {
			return nil, fail
		}
		return []rblndevice.Device{npuEntry("rbln0", "0000:17:00.0")}, nil
	})
	rescan(t, st)

	fail = errors.New("exit status 1")
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{rblnVfioDevice("0000:17:00.0"), nativeDevice("0000:27:00.0", "0")})
	buf.Reset()
	resources, changed, err := st.RescanDevices(context.Background())
	if !errors.Is(err, fail) || changed {
		t.Fatalf("partial tick: changed=%v err=%v", changed, err)
	}
	if got := publishedNames(resources); !sameNames(got, []string{"rbln0"}) {
		t.Fatalf("published %v, want the previous complete snapshot", got)
	}
	failure := logtest.Find(logtest.Lines(t, buf), "NPU enumeration failed, keeping last inventory")
	if failure == nil || failure["level"] != "error" || failure["err"] != "exit status 1" || failure["impact"] == nil {
		t.Fatalf("first failure must be an error with impact: %s", buf.String())
	}

	buf.Reset()
	if _, changed, err := st.RescanDevices(context.Background()); !errors.Is(err, fail) || changed || buf.Len() != 0 {
		t.Errorf("a persisting failure must stay silent: %s", buf.String())
	}

	fail = nil
	st.inventoryFn = inventoryOf(npuEntry("rbln1", "0000:27:00.0"))
	buf.Reset()
	if !rescan(t, st) {
		t.Error("recovery must publish the new complete snapshot")
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"rbln1", "vfio-0000-17-00-0"}) {
		t.Fatalf("published after recovery: %v", got)
	}
	if logtest.Find(logtest.Lines(t, buf), "NPU enumeration recovered") == nil {
		t.Errorf("recovery must be logged once: %s", buf.String())
	}
}

// A failed query is not evidence that devices disappeared.
func TestRescanDevicesKeepsInventoryWhileSysfsUnreadable(t *testing.T) {
	buf := logtest.Capture(t, "info")
	root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")})
	st := scanState(root, inventoryOf(npuEntry("rbln0", "0000:17:00.0")))
	rescan(t, st)

	st.sysfsPCIDevicesRoot = filepath.Join(t.TempDir(), "missing")
	buf.Reset()
	resources, changed, err := st.RescanDevices(context.Background())
	if err == nil || changed {
		t.Fatalf("unreadable sysfs: changed=%v err=%v", changed, err)
	}
	if got := publishedNames(resources); !sameNames(got, []string{"rbln0"}) {
		t.Fatalf("published %v, want the kept inventory", got)
	}
	if failure := logtest.Find(logtest.Lines(t, buf), "Device scan failed, keeping last inventory"); failure == nil || failure["level"] != "error" || failure["impact"] == nil {
		t.Fatalf("first failure must be an error with impact: %s", buf.String())
	}

	buf.Reset()
	if _, changed, err := st.RescanDevices(context.Background()); err == nil || changed || buf.Len() != 0 {
		t.Errorf("a persisting failure must stay silent: err=%v %s", err, buf.String())
	}

	st.sysfsPCIDevicesRoot = root
	buf.Reset()
	if rescan(t, st) {
		t.Error("recovery with the same inventory must not republish")
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"rbln0"}) {
		t.Fatalf("published after recovery: %v", got)
	}
	if logtest.Find(logtest.Lines(t, buf), "Device scan recovered") == nil {
		t.Errorf("recovery must be logged once: %s", buf.String())
	}
}

// At debug level every tick leaves one record saying what the scan saw, so a
// quiet loop and a dead loop can be told apart.
func TestRescanDevicesEmitsOneDebugRecordPerTick(t *testing.T) {
	buf := logtest.Capture(t, "debug")
	root, inventory := vf4Layout(t)
	st := scanState(root, inventory)
	rescan(t, st)

	buf.Reset()
	rescan(t, st)
	var records []map[string]any
	for _, line := range logtest.Lines(t, buf) {
		if line["msg"] == "Scanned device inventory" {
			records = append(records, line)
		}
	}
	if len(records) != 1 {
		t.Fatalf("got %d scan records, want 1: %s", len(records), buf.String())
	}
	r := records[0]
	if r["level"] != "debug" || r["npuDeviceCount"] != float64(4) || r["vfioDeviceCount"] != float64(0) ||
		r["nativeCandidates"] != float64(4) || r["rblnSmiRun"] != true || r["changed"] != false || r["durationMs"] == nil {
		t.Errorf("unexpected scan record: %v", r)
	}
	if skipped, _ := r["skipped"].(map[string]any); skipped["rbln0"] == nil {
		t.Errorf("scan record must carry the skip reasons: %v", r["skipped"])
	}
}

// The slow-tick warning is the only default-level signal that the interval
// is too aggressive for a node; it fires on the transition, not every tick.
func TestObserveScanDurationWarnsOnceWhenSlowerThanInterval(t *testing.T) {
	buf := logtest.Capture(t, "info")
	st := &DeviceState{}
	interval := 5 * time.Second

	st.observeScanDuration(context.Background(), 2*time.Second, interval)
	if buf.Len() != 0 {
		t.Fatalf("a fast tick must stay silent: %s", buf.String())
	}

	st.observeScanDuration(context.Background(), 7*time.Second, interval)
	slow := logtest.Find(logtest.Lines(t, buf), "Device scan slower than its interval")
	if slow == nil || slow["level"] != "warn" || slow["durationMs"] != float64(7000) || slow["interval"] != "5s" {
		t.Fatalf("slow tick must be a warn naming the duration and interval: %s", buf.String())
	}

	buf.Reset()
	st.observeScanDuration(context.Background(), 8*time.Second, interval)
	if buf.Len() != 0 {
		t.Errorf("a persisting slow tick must stay silent: %s", buf.String())
	}

	buf.Reset()
	st.observeScanDuration(context.Background(), time.Second, interval)
	if logtest.Find(logtest.Lines(t, buf), "Device scan duration back under its interval") == nil {
		t.Errorf("recovery must be logged once: %s", buf.String())
	}
}

// A claim allocated to a device that a later scan removed must fail Prepare
// rather than be prepared against stale state; the consumer has to be
// recreated so the scheduler reallocates it.
func TestPrepareRejectsDeviceRemovedByRescan(t *testing.T) {
	st := prepareStateFor(t, "/dev/null")
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:4d:00.0", "0")})
	st.inventoryFn = inventoryOf(npuEntry("null", "0000:4d:00.0"))
	rescan(t, st)
	if _, err := st.Prepare(context.Background(), allocatedClaim()); err != nil {
		t.Fatalf("prepare against the scanned inventory: %v", err)
	}
	if err := st.Unprepare(context.Background(), string(allocatedClaim().UID)); err != nil {
		t.Fatal(err)
	}

	// The PF now hosts VFs: it leaves the inventory on the next scan.
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:4d:00.0", "4")})
	if !rescan(t, st) {
		t.Fatal("hosting PF was not removed by the rescan")
	}
	_, err := st.Prepare(context.Background(), allocatedClaim())
	if err == nil || !strings.Contains(err.Error(), "not allocatable") {
		t.Fatalf("prepare after removal = %v, want a not-allocatable error", err)
	}
}

// Startup applies the same gate as the scan loop: a VF-passthrough node has
// no rbln-smi binary, so the plugin must not try it and must not describe
// its absence as a failure.
func TestNewDeviceStateSkipsRblnSmiOnPassthroughNode(t *testing.T) {
	buf := logtest.Capture(t, "info")
	calls := 0
	config := &Config{
		flags:               &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot: writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "4"), vfioVF("0000:17:00.1", "0000:17:00.0")}),
		inventoryFn: func(context.Context) ([]rblndevice.Device, error) {
			calls++
			return nil, errors.New("rbln-smi is not installed on this node")
		},
	}
	st, err := NewDeviceState(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("rbln-smi ran %d times at startup on a VF-passthrough node", calls)
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"vfio-0000-17-00-1-vf4"}) {
		t.Fatalf("published %v, want only the passthrough VF", got)
	}
	for _, line := range logtest.Lines(t, buf) {
		if line["level"] == "error" {
			t.Errorf("startup on a passthrough node must not report an error: %v", line)
		}
	}
}

func TestNewDeviceStateRecoversFromNativeInventoryFailure(t *testing.T) {
	ctx := context.Background()
	st, err := NewDeviceState(ctx, &Config{
		flags:               &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot: writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}),
		inventoryFn:         func(context.Context) ([]rblndevice.Device, error) { return nil, errors.New("driver initializing") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.allocatable) != 0 {
		t.Fatal("failed startup discovery must publish no native devices")
	}
	st.inventoryFn = inventoryOf(npuEntry("rbln0", "0000:17:00.0"))
	if !rescan(t, st) || len(st.allocatable) != 1 {
		t.Fatal("native discovery did not recover without a restart")
	}
}

func TestPrepareRetriesCommonCDIAfterLateNativeDiscovery(t *testing.T) {
	ctx := context.Background()
	st, err := NewDeviceState(ctx, &Config{
		flags:               &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot: writeFakeSysfs(t, []fakePCIDevice{rblnVfioDevice("0000:4d:00.0")}),
		inventoryFn:         inventoryOf(),
	})
	if err != nil {
		t.Fatal(err)
	}
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:4d:00.0", "0")})
	st.inventoryFn = inventoryOf(npuEntry("null", "0000:4d:00.0"))
	groupCalls := 0
	st.rsdGroupFn = func([]string) (string, error) { groupCalls++; return "/dev/null", nil }
	rescan(t, st)
	if _, err := st.Prepare(ctx, allocatedClaim()); err == nil || !strings.Contains(err.Error(), "common CDI") {
		t.Fatalf("Prepare without the toolkit = %v", err)
	}
	if groupCalls != 0 {
		t.Fatal("missing runtime must not recreate an RSD group")
	}

	if err := os.WriteFile(filepath.Join(st.cdi.root, rblnRuntimeSpecFile), []byte(runtimeSpecFixture), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, err := st.Prepare(ctx, allocatedClaim())
	if err != nil {
		t.Fatal(err)
	}
	if groupCalls != 1 {
		t.Fatalf("RSD group calls = %d, want 1", groupCalls)
	}
	// Use a fresh reader like the runtime; the writer's filesystem watcher
	// can lag behind WriteSpec and auto-refresh does not force a rescan.
	cache, err := cdiapi.NewCache(cdiapi.WithSpecDirs(st.cdi.root), cdiapi.WithAutoRefresh(false))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Refresh(); err != nil {
		t.Fatal(err)
	}
	for _, d := range prepared {
		for _, id := range d.CdiDeviceIds {
			if cache.GetDevice(id) == nil {
				t.Fatalf("Prepare referenced missing CDI device %s", id)
			}
		}
	}
}

func TestRescanFailurePreservesPreparedClaims(t *testing.T) {
	ctx := context.Background()
	st := prepareStateFor(t, "/dev/null")
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:4d:00.0", "0")})
	inventory := inventoryOf(npuEntry("null", "0000:4d:00.0"))
	st.inventoryFn = inventory
	rescan(t, st)
	claim := allocatedClaim()
	if _, err := st.Prepare(ctx, claim); err != nil {
		t.Fatal(err)
	}
	st.inventoryFn = func(context.Context) ([]rblndevice.Device, error) { return nil, errors.New("temporary SMI failure") }
	if _, changed, err := st.RescanDevices(ctx); err == nil || changed || len(st.allocatable) != 1 {
		t.Fatalf("failed scan: changed=%v err=%v devices=%v", changed, err, st.allocatable)
	}
	st.rsdGroupFn = func([]string) (string, error) {
		t.Fatal("checkpoint replay must not recreate the group")
		return "", nil
	}
	if prepared, err := st.Prepare(ctx, claim); err != nil || len(prepared) != 1 {
		t.Fatalf("checkpoint replay after discovery failure: %v, %v", prepared, err)
	}
	st.inventoryFn = inventory
	if rescan(t, st) {
		t.Fatal("recovery with unchanged inventory must not republish")
	}
}

func TestSuccessfulEmptyScanRemovesDevices(t *testing.T) {
	st := scanState(writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}), inventoryOf(npuEntry("rbln0", "0000:17:00.0")))
	rescan(t, st)
	st.sysfsPCIDevicesRoot = writeFakeSysfs(t, nil)
	if !rescan(t, st) || len(st.allocatable) != 0 {
		t.Fatal("a successful empty scan must remove devices")
	}
}
