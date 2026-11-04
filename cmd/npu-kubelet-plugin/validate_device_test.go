package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	rblndevice "github.com/rbln-sw/rblnlib-go/pkg/device"
	resourceapi "k8s.io/api/resource/v1"
)

func TestValidateAllocatedDevice(t *testing.T) {
	pf, vf := "0000:17:00.0", "0000:17:00.1"
	for _, mode := range []string{"native-pf", "native-vf", "vfio-pf", "vfio-vf"} {
		t.Run(mode, func(t *testing.T) {
			for _, change := range []string{"unchanged", "removed", "unbound", "rebound", "vendor", "partition", "iommu", "shared-group"} {
				t.Run(change, func(t *testing.T) {
					vfio := mode == "vfio-pf" || mode == "vfio-vf"
					isVF := mode == "native-vf" || mode == "vfio-vf"
					if !vfio && (change == "iommu" || change == "shared-group") {
						t.Skip("VFIO-only check")
					}
					bus := pf
					layout := []fakePCIDevice{nativeDevice(pf, "0")}
					if isVF {
						bus = vf
						layout = []fakePCIDevice{nativeDevice(pf, "4"), nativeVF(vf, pf)}
					}
					if vfio {
						layout[len(layout)-1].driver = vfioPCIDriverName
					}
					root := writeFakeSysfs(t, layout)
					var devices []resourceapi.Device
					var err error
					if vfio {
						devices, _, err = enumerateVfioDevices(context.Background(), root, nil)
					} else {
						devices, _, err = npuDevicesFromInventory(context.Background(), root, []rblndevice.Device{npuEntry("null", bus)})
					}
					if err != nil || len(devices) != 1 {
						t.Fatalf("discovery: %v, %v", devices, err)
					}
					devPath := filepath.Join(root, bus)
					switch change {
					case "removed":
						if err := os.Remove(devPath); err != nil {
							t.Fatal(err)
						}
					case "unbound", "rebound":
						if err := os.Remove(filepath.Join(devPath, "driver")); err != nil {
							t.Fatal(err)
						}
						if change == "rebound" {
							driver := vfioPCIDriverName
							if vfio {
								driver = nativePCIDriverName
							}
							if err := os.Symlink("../drivers/"+driver, filepath.Join(devPath, "driver")); err != nil {
								t.Fatal(err)
							}
						}
					case "vendor":
						if err := os.WriteFile(filepath.Join(devPath, "vendor"), []byte("0x10de"), 0600); err != nil {
							t.Fatal(err)
						}
					case "partition":
						count := "4"
						if isVF {
							count = "1"
						}
						if err := os.WriteFile(filepath.Join(root, pf, "sriov_numvfs"), []byte(count), 0600); err != nil {
							t.Fatal(err)
						}
					case "iommu":
						group := filepath.Join(t.TempDir(), "987654")
						if err := os.MkdirAll(filepath.Join(group, "devices", bus), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(filepath.Join(devPath, "iommu_group")); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink(group, filepath.Join(devPath, "iommu_group")); err != nil {
							t.Fatal(err)
						}
					case "shared-group":
						if err := os.Mkdir(filepath.Join(devPath, "iommu_group", "devices", "0000:ff:00.0"), 0700); err != nil {
							t.Fatal(err)
						}
					}
					err = validateAllocatedDevice(root, devices[0])
					if (err != nil) != (change != "unchanged") {
						t.Fatalf("validation after %s: %v", change, err)
					}
				})
			}
		})
	}
}

func TestRepartitionRejectsOldAllocationWithoutClassPolicy(t *testing.T) {
	pf, vf := "0000:17:00.0", "0000:17:00.1"
	for _, vfio := range []bool{false, true} {
		name := "native"
		if vfio {
			name = "vfio"
		}
		t.Run(name, func(t *testing.T) {
			st := prepareStateFor(t, "/dev/null")
			device := nativeVF(vf, pf)
			if vfio {
				device.driver = vfioPCIDriverName
			}
			st.sysfsPCIDevicesRoot = writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), device})
			st.inventoryFn = inventoryOf(npuEntry("null", vf))
			rescan(t, st)
			oldName := publishedNames(st.DriverResources())[0]
			claim := allocatedClaim()
			claim.Status.Allocation.Devices.Results[0].Device = oldName
			claim.Spec.Devices.Requests = []resourceapi.DeviceRequest{{Name: "npu", Exactly: &resourceapi.ExactDeviceRequest{DeviceClassName: "custom.example.com"}}}
			if err := os.WriteFile(filepath.Join(st.sysfsPCIDevicesRoot, pf, "sriov_numvfs"), []byte("1"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Prepare(context.Background(), claim); err == nil {
				t.Fatal("Prepare accepted stale live partition state")
			}
			rescan(t, st)
			newName := publishedNames(st.DriverResources())[0]
			if newName == oldName {
				t.Fatal("repartition reused the old device identity")
			}
			if _, err := st.Prepare(context.Background(), claim); err == nil {
				t.Fatal("old allocation was reinterpreted as VF1")
			}
			if err := validateAllocatedDevice(st.sysfsPCIDevicesRoot, st.allocatable[newName]); err != nil {
				t.Fatal(err)
			}
			if rescan(t, st) {
				t.Fatal("an unchanged scan changed identity")
			}
			if !vfio {
				claim.Status.Allocation.Devices.Results[0].Device = newName
				if _, err := st.Prepare(context.Background(), claim); err != nil {
					t.Fatalf("new VF1 allocation did not prepare: %v", err)
				}
			}
		})
	}
}

func TestVfioDiscoveryRequiresIndependentGroups(t *testing.T) {
	pf := "0000:17:00.0"
	a, b := vfioVF("0000:17:00.1", pf), vfioVF("0000:17:00.2", pf)
	root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), a, b})
	devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
	if err != nil || len(devices) != 2 {
		t.Fatalf("independent VFs: %v, %v", devices, err)
	}
	b.iommuGroup = a.iommuGroup
	root = writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, "4"), a, b})
	devices, _, err = enumerateVfioDevices(context.Background(), root, nil)
	if err != nil || len(devices) != 0 {
		t.Fatalf("shared group was advertised as independent devices: %v, %v", devices, err)
	}
}
