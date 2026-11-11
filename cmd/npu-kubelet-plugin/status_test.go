package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
	rblndevice "github.com/rbln-sw/rblnlib-go/pkg/device"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
)

func writeDeviceStatus(t *testing.T, root, name, status string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceHealthFromSysfsStatus(t *testing.T) {
	for _, tc := range []struct {
		name, raw, status string
		unhealthy         bool
	}{
		{name: "ready", raw: " 0\n", status: "READY"},
		{name: "busy", raw: "1", status: "BUSY", unhealthy: true},
		{name: "probing", raw: "2", status: "PROBING", unhealthy: true},
		{name: "init", raw: "3", status: "INIT", unhealthy: true},
		{name: "fault", raw: "4", status: "FAULT", unhealthy: true},
		{name: "finish", raw: "5", status: "FINISH", unhealthy: true},
		{name: "unknown", raw: "9", status: "UNKNOWN(9)", unhealthy: true},
		{name: "negative", raw: "-1", status: "UNKNOWN(-1)", unhealthy: true},
		{name: "invalid", raw: "n/a", status: deviceStatusUnreadable},
		{name: "empty", status: deviceStatusUnreadable},
		{name: "missing", status: deviceStatusUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.name != "missing" {
				writeDeviceStatus(t, root, "rbln0", tc.raw)
			}
			got := healthForDevice(context.Background(), root, "rbln0")
			if got.status != tc.status || got.unhealthy != tc.unhealthy {
				t.Fatalf("health = %+v, want status=%s unhealthy=%v", got, tc.status, tc.unhealthy)
			}
		})
	}
}

func TestStartupFiltersUnhealthyNativeDevicesOnly(t *testing.T) {
	statusRoot := t.TempDir()
	writeDeviceStatus(t, statusRoot, "rbln0", "4")
	writeDeviceStatus(t, statusRoot, "rbln1-0", "4")
	// This is a DRA name, not a native device node. VFIO must not read it.
	writeDeviceStatus(t, statusRoot, "vfio-0000-65-00-0", "4")
	st, err := NewDeviceState(context.Background(), &Config{
		flags: &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot: writeFakeSysfs(t, []fakePCIDevice{
			nativeDevice("0000:17:00.0", "0"), nativeDevice("0000:27:00.0", "1"),
			nativeVF("0000:27:00.1", "0000:27:00.0"), rblnVfioDevice("0000:65:00.0"),
		}),
		sysfsRebellionsClassRoot: statusRoot,
		inventoryFn:              inventoryOf(npuEntry("rbln0", "0000:17:00.0"), npuEntry("rbln1-0", "0000:27:00.1")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"vfio-0000-65-00-0"}) {
		t.Fatalf("published devices = %v; want only VFIO", got)
	}
	if len(st.npuDevices) != 2 || len(st.allocatable) != 1 {
		t.Fatalf("discovered=%d allocatable=%d; keep unhealthy identities for recovery", len(st.npuDevices), len(st.allocatable))
	}
	writeDeviceStatus(t, statusRoot, "rbln1-0", "0")
	if !rescan(t, st) {
		t.Fatal("VF recovery must republish")
	}
	if _, ok := st.allocatable["rbln1-0-vf1"]; !ok {
		t.Fatal("VF status must use its native node name, without the profile suffix")
	}
}

func TestRescanWithdrawsAndRestoresOnlyUnhealthyDevice(t *testing.T) {
	buf := logtest.Capture(t, "info")
	st := scanState(writeFakeSysfs(t, []fakePCIDevice{
		nativeDevice("0000:17:00.0", "0"), nativeDevice("0000:27:00.0", "0"),
	}), inventoryOf(npuEntry("rbln0", "0000:17:00.0"), npuEntry("rbln1", "0000:27:00.0")))
	st.sysfsRebellionsClassRoot = t.TempDir()
	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "0")
	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln1", "0")
	rescan(t, st)

	buf.Reset()
	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "4")
	if !rescan(t, st) || !sameNames(publishedNames(st.DriverResources()), []string{"rbln1"}) {
		t.Fatal("a fault must remove only the unhealthy device")
	}
	line := logtest.Find(logtest.Lines(t, buf), "NPU is unhealthy, excluding from new allocations")
	if line == nil || line["level"] != "warn" || line["status"] != "FAULT" || line["device"] != "rbln0" {
		t.Fatalf("missing actionable health transition: %s", buf.String())
	}
	buf.Reset()
	if rescan(t, st) || buf.Len() != 0 {
		t.Fatalf("unchanged fault must not republish or repeat warnings: %s", buf.String())
	}

	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "0")
	if !rescan(t, st) || !sameNames(publishedNames(st.DriverResources()), []string{"rbln0", "rbln1"}) {
		t.Fatal("recovery must restore the same device identity")
	}
	if logtest.Find(logtest.Lines(t, buf), "NPU is available for new allocations again") == nil {
		t.Fatalf("missing recovery record: %s", buf.String())
	}
}

// Read failures must not turn a previously observed fault into recovery.
// Cover both normal discovery and a tick that reuses its last inventory.
func TestKnownFaultSurvivesUnreadableStatus(t *testing.T) {
	for _, inventoryFails := range []bool{false, true} {
		for _, failure := range []string{"missing", "malformed", "read-error"} {
			name := failure
			if inventoryFails {
				name += "/inventory-failure"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				buf := logtest.Capture(t, "info")
				st := scanState(writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}),
					inventoryOf(npuEntry("rbln0", "0000:17:00.0")))
				st.sysfsRebellionsClassRoot = t.TempDir()
				writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "0")
				rescan(t, st)
				writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "4")
				rescan(t, st)
				if len(st.allocatable) != 0 {
					t.Fatal("faulted device must be withdrawn")
				}
				if inventoryFails {
					st.inventoryFn = func(context.Context) ([]rblndevice.Device, error) { return nil, errors.New("SMI failed") }
				}
				statusPath := filepath.Join(st.sysfsRebellionsClassRoot, "rbln0", "status")
				if failure == "malformed" {
					writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "n/a")
				} else {
					if err := os.Remove(statusPath); err != nil {
						t.Fatal(err)
					}
					if failure == "read-error" {
						if err := os.Mkdir(statusPath, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				buf.Reset()
				for i := 0; i < 2; i++ {
					resources, changed, err := st.RescanDevices(ctx)
					if (err != nil) != inventoryFails || changed || len(publishedNames(resources)) != 0 || len(st.allocatable) != 0 {
						t.Fatalf("unreadable status republished a known fault: devices=%v changed=%v err=%v", publishedNames(resources), changed, err)
					}
					if logtest.Find(logtest.Lines(t, buf), "NPU is available for new allocations again") != nil {
						t.Fatalf("read failure must not log recovery: %s", buf.String())
					}
				}
				if failure == "read-error" {
					if err := os.Remove(statusPath); err != nil {
						t.Fatal(err)
					}
				}
				writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "0")
				resources, changed, err := st.RescanDevices(ctx)
				if (err != nil) != inventoryFails || !changed || !sameNames(publishedNames(resources), []string{"rbln0"}) {
					t.Fatalf("READY did not restore the device: devices=%v changed=%v err=%v", publishedNames(resources), changed, err)
				}
				if logtest.Find(logtest.Lines(t, buf), "NPU is available for new allocations again") == nil {
					t.Fatalf("READY recovery must be logged: %s", buf.String())
				}
			})
		}
	}
}

func TestMissingStatusWithoutKnownFaultRemainsAllocatable(t *testing.T) {
	ctx := context.Background()
	statusRoot := t.TempDir()
	st, err := NewDeviceState(ctx, &Config{
		flags:                    &Flags{nodeName: "node-1", cdiRoot: t.TempDir(), kubeletPluginsDirectoryPath: t.TempDir()},
		sysfsPCIDevicesRoot:      writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}),
		sysfsRebellionsClassRoot: statusRoot,
		inventoryFn:              inventoryOf(npuEntry("rbln0", "0000:17:00.0")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"rbln0"}) {
		t.Fatalf("missing status without prior fault must preserve capacity: %v", got)
	}
	if rescan(t, st) {
		t.Fatal("unchanged missing status must not republish")
	}
	writeDeviceStatus(t, statusRoot, "rbln0", "0")
	if rescan(t, st) {
		t.Fatal("observing READY must not change already available capacity")
	}
	if err := os.Remove(filepath.Join(statusRoot, "rbln0", "status")); err != nil {
		t.Fatal(err)
	}
	if rescan(t, st) || len(st.allocatable) != 1 {
		t.Fatal("an unreadable status without a prior fault must preserve capacity")
	}
}

func TestInventoryFailureStillRefreshesDeviceHealth(t *testing.T) {
	for _, failure := range []string{"rbln-smi", "PCI sysfs"} {
		t.Run(failure, func(t *testing.T) {
			st := scanState(writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0")}),
				inventoryOf(npuEntry("rbln0", "0000:17:00.0")))
			st.sysfsRebellionsClassRoot = t.TempDir()
			writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "0")
			rescan(t, st)
			if failure == "rbln-smi" {
				st.inventoryFn = func(context.Context) ([]rblndevice.Device, error) { return nil, errors.New("SMI failed") }
			} else {
				st.sysfsPCIDevicesRoot = filepath.Join(t.TempDir(), "unreadable")
			}
			writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "4")
			resources, changed, err := st.RescanDevices(context.Background())
			if err == nil || !changed || len(publishedNames(resources)) != 0 || len(st.allocatable) != 0 {
				t.Fatalf("fault during discovery failure: devices=%v changed=%v err=%v", publishedNames(resources), changed, err)
			}
			if len(st.npuDevices) != 1 {
				t.Fatal("inventory failure must preserve known device identities")
			}
			if _, changed, err := st.RescanDevices(context.Background()); err == nil || changed {
				t.Fatalf("unchanged fault: changed=%v err=%v", changed, err)
			}
			writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "rbln0", "0")
			resources, changed, err = st.RescanDevices(context.Background())
			if err == nil || !changed || !sameNames(publishedNames(resources), []string{"rbln0"}) {
				t.Fatalf("recovery during discovery failure: devices=%v changed=%v err=%v", publishedNames(resources), changed, err)
			}
		})
	}
}

func TestPrepareRejectsFaultBetweenScansBeforeSideEffects(t *testing.T) {
	for _, vf := range []bool{false, true} {
		t.Run(map[bool]string{false: "PF", true: "VF"}[vf], func(t *testing.T) {
			st := prepareStateFor(t, "/dev/null")
			st.sysfsRebellionsClassRoot = t.TempDir()
			claim := allocatedClaim()
			if vf {
				pf, bus := "0000:4d:00.0", "0000:4d:00.1"
				st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), nativeVF(bus, pf)})
				d := st.allocatable["null"]
				d.Name = "null-vf4"
				d.Attributes[pciBusIDAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(bus)}
				d.Attributes[deviceTypeAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(deviceTypeNpuVF)}
				(sriovInfo{parentPCIBusID: pf, numVFs: 4}).setAttributes(d.Attributes)
				st.allocatable = AllocatableDevices{d.Name: d}
				claim.Status.Allocation.Devices.Results[0].Device = d.Name
			}
			allocated := st.allocatable[claim.Status.Allocation.Devices.Results[0].Device]
			st.inventoryFn = inventoryOf(npuEntry("null", deviceStringAttr(allocated, pciBusIDAttributeKey)))
			rescan(t, st)
			st.rsdGroupFn = func([]string) (string, error) {
				t.Fatal("an unhealthy device must be rejected before RSD creation")
				return "", nil
			}
			writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "null", "4")
			if _, err := st.Prepare(context.Background(), claim); err == nil || !strings.Contains(err.Error(), "unhealthy: FAULT") {
				t.Fatalf("Prepare = %v, want unhealthy FAULT", err)
			}
			// A fault first observed by Prepare must also survive a read
			// failure on a retry before the next periodic scan.
			if err := os.Remove(filepath.Join(st.sysfsRebellionsClassRoot, "null", "status")); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if _, err := st.Prepare(context.Background(), claim); err == nil || !strings.Contains(err.Error(), "is unhealthy") {
					t.Fatalf("Prepare retry with unreadable status = %v, want unhealthy error", err)
				}
			}
			cp := newCheckpoint()
			if err := st.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, cp); err != nil {
				t.Fatal(err)
			}
			if len(cp.V1.PreparedClaims) != 0 {
				t.Fatal("failed Prepare must not record a prepared claim")
			}
			files, err := os.ReadDir(st.cdi.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.Name() != rblnRuntimeSpecFile {
					t.Fatalf("unhealthy Prepare wrote CDI state: %s", file.Name())
				}
			}
			if !rescan(t, st) || len(publishedNames(st.DriverResources())) != 0 {
				t.Fatal("scan with unreadable status must withdraw a fault first observed by Prepare")
			}
			writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "null", "0")
			if !rescan(t, st) || len(st.allocatable) != 1 {
				t.Fatal("scan must restore the device after observing READY")
			}
			st.rsdGroupFn = func([]string) (string, error) { return "/dev/null", nil }
			if _, err := st.Prepare(context.Background(), claim); err != nil {
				t.Fatalf("Prepare retry after observing READY: %v", err)
			}
		})
	}
}

func TestHealthChangesPreservePreparedClaimAndCleanup(t *testing.T) {
	ctx := context.Background()
	st := prepareStateFor(t, "/dev/null")
	st.sysfsRebellionsClassRoot = t.TempDir()
	st.inventoryFn = inventoryOf(npuEntry("null", "0000:4d:00.0"))
	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "null", "0")
	rescan(t, st)
	claim := allocatedClaim()
	prepared, err := st.Prepare(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	specName := cdiapi.GenerateTransientSpecName(st.cdi.vendor(), st.cdi.class, string(claim.UID)) + ".yaml"
	specPath := filepath.Join(st.cdi.root, specName)
	before, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "null", "4")
	if !rescan(t, st) {
		t.Fatal("fault must withdraw prepared device from new allocations")
	}
	fresh := claim.DeepCopy()
	fresh.UID = "new-claim"
	if _, err := st.Prepare(ctx, fresh); err == nil || !strings.Contains(err.Error(), "not allocatable") {
		t.Fatalf("fresh Prepare after fault = %v", err)
	}
	st.rsdGroupFn = func([]string) (string, error) {
		t.Fatal("checkpoint replay must not recreate RSD state")
		return "", nil
	}
	replayed, err := st.Prepare(ctx, claim)
	if err != nil || !reflect.DeepEqual(prepared, replayed) {
		t.Fatalf("checkpoint replay after fault = %v, %v", replayed, err)
	}
	after, err := os.ReadFile(specPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("health change must preserve the claim CDI spec: %v", err)
	}
	if err := st.Unprepare(ctx, string(claim.UID)); err != nil {
		t.Fatalf("Unprepare must work while device is unhealthy: %v", err)
	}
	if _, err := os.Stat(specPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Unprepare must remove the claim CDI spec: %v", err)
	}
	writeDeviceStatus(t, st.sysfsRebellionsClassRoot, "null", "0")
	rescan(t, st)
	st.rsdGroupFn = func([]string) (string, error) { return "/dev/null", nil }
	if _, err := st.Prepare(ctx, fresh); err != nil {
		t.Fatalf("fresh Prepare after recovery: %v", err)
	}
}
