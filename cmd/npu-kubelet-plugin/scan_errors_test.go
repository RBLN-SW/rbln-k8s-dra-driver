package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Required reads must not turn a temporary observation failure into removal
// of previously published devices. A missing PCI entry is tested separately.
func TestRequiredSysfsFailuresKeepCompleteSnapshot(t *testing.T) {
	pf, vf := "0000:17:00.0", "0000:17:00.1"
	for _, vfio := range []bool{false, true} {
		for _, attribute := range []string{"vendor", "driver", "parent-mode", "missing-parent-mode", "malformed-mode", "iommu-members"} {
			name := "native/" + attribute
			if vfio {
				name = "vfio/" + attribute
			}
			t.Run(name, func(t *testing.T) {
				if !vfio && attribute == "iommu-members" {
					t.Skip("VFIO-only attribute")
				}
				function := nativeVF(vf, pf)
				if vfio {
					function.driver = vfioPCIDriverName
				}
				root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), function})
				st := scanState(root, inventoryOf(npuEntry("rbln0-0", vf)))
				rescan(t, st)
				before := st.DriverResources()
				badPath := filepath.Join(root, vf, attribute)
				switch attribute {
				case "parent-mode", "missing-parent-mode", "malformed-mode":
					badPath = filepath.Join(root, pf, "sriov_numvfs")
				case "iommu-members":
					badPath = filepath.Join(root, vf, "iommu_group", "devices")
				}
				switch attribute {
				case "iommu-members":
					// Move the real directory aside and replace it with a file.
					if err := os.Rename(badPath, badPath+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(badPath, nil, 0600); err != nil {
						t.Fatal(err)
					}
				case "malformed-mode":
					if err := os.WriteFile(badPath, []byte("unreadable-value"), 0600); err != nil {
						t.Fatal(err)
					}
				default:
					if err := os.Remove(badPath); err != nil {
						t.Fatal(err)
					}
					// A directory makes ReadFile/Readlink fail even when tests run as root.
					if attribute != "missing-parent-mode" {
						if err := os.Mkdir(badPath, 0700); err != nil {
							t.Fatal(err)
						}
					}
				}
				after, changed, err := st.RescanDevices(context.Background())
				if err == nil || changed || !reflect.DeepEqual(before, after) {
					t.Fatalf("failed read replaced snapshot: changed=%v err=%v", changed, err)
				}
			})
		}
	}
}

func TestUnknownVendorReadFailureDoesNotFreezeDiscovery(t *testing.T) {
	for _, vendor := range []string{"0x10de", rblnPCIVendorID} {
		for _, vfio := range []bool{false, true} {
			name := "native/" + vendor
			if vfio {
				name = "vfio/" + vendor
			}
			t.Run(name, func(t *testing.T) {
				d := nativeDevice("0000:17:00.0", "0")
				if vfio {
					d.driver = vfioPCIDriverName
				}
				unknown := rblnVfioDevice("0000:27:00.0")
				unknown.vendor = vendor
				root := writeFakeSysfs(t, []fakePCIDevice{d, unknown})
				vendorPath := filepath.Join(root, unknown.busID, "vendor")
				if err := os.Remove(vendorPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(vendorPath, 0700); err != nil {
					t.Fatal(err)
				}
				st := scanState(root, inventoryOf(npuEntry("rbln0", d.busID)))
				if !rescan(t, st) || len(st.allocatable) != 1 {
					t.Fatalf("unknown vendor blocked discovery: %v", st.allocatable)
				}
				if rescan(t, st) {
					t.Fatal("unchanged inventory republished")
				}
				if err := os.Remove(vendorPath); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(vendorPath, []byte(vendor), 0600); err != nil {
					t.Fatal(err)
				}
				changed := rescan(t, st)
				if changed != (vendor == rblnPCIVendorID) {
					t.Fatalf("vendor recovery changed=%v for vendor %s", changed, vendor)
				}
			})
		}
	}
}

func TestOptionalSysfsAttributeDoesNotBlockDiscovery(t *testing.T) {
	d := rblnVfioDevice("0000:17:00.0")
	root := writeFakeSysfs(t, []fakePCIDevice{d})
	if err := os.Remove(filepath.Join(root, d.busID, "numa_node")); err != nil {
		t.Fatal(err)
	}
	devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
	if err != nil || len(devices) != 1 {
		t.Fatalf("optional NUMA read: %v, %v", devices, err)
	}
	if _, found := devices[0].Attributes[numaNodeAttributeKey]; found {
		t.Fatal("unknown NUMA must be omitted")
	}
}

func TestRemovedNativeFunctionDoesNotBecomeReadFailure(t *testing.T) {
	root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice("0000:17:00.0", "0"), nativeDevice("0000:27:00.0", "0")})
	st := scanState(root, inventoryOf(npuEntry("rbln0", "0000:17:00.0"), npuEntry("rbln1", "0000:27:00.0")))
	rescan(t, st)
	if err := os.Remove(filepath.Join(root, "0000:17:00.0")); err != nil {
		t.Fatal(err)
	}
	if !rescan(t, st) {
		t.Fatal("actual removal was not published")
	}
	if got := publishedNames(st.DriverResources()); !sameNames(got, []string{"rbln1"}) {
		t.Fatalf("remaining devices: %v", got)
	}
}
