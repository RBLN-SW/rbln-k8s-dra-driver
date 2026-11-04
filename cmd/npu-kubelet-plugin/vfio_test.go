/*
 * Copyright 2025 The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"

	"github.com/RBLN-SW/k8s-dra-driver-npu/internal/logtest"
)

type fakePCIDevice struct {
	busID      string
	vendor     string
	class      string
	device     string
	driver     string
	iommuGroup string
	numaNode   string
	rootPort   string
	physfnOf   string // parent PF bus ID; physfn then points at a real PF entry
	numVFs     string
}

// writeFakeSysfs lays out a sysfs-like tree:
//
//	<dir>/sys/devices/pci0000:00/<rootPort>/<busID>/{vendor,class,device,numa_node}
//	<dir>/sys/devices/pci0000:00/<rootPort>/<busID>/driver -> .../drivers/<driver>
//	<dir>/sys/devices/pci0000:00/<rootPort>/<busID>/iommu_group -> .../iommu_groups/<group>
//	<dir>/sys/bus/pci/devices/<busID> -> physical path (as on real systems)
func writeFakeSysfs(t *testing.T, devices []fakePCIDevice) string {
	t.Helper()

	dir := t.TempDir()
	busDir := filepath.Join(dir, "sys", "bus", "pci", "devices")
	if err := os.MkdirAll(busDir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, dev := range devices {
		devDir := filepath.Join(dir, "sys", "devices", "pci0000:00", dev.rootPort, dev.busID)
		if err := os.MkdirAll(devDir, 0o755); err != nil {
			t.Fatal(err)
		}

		files := map[string]string{
			"vendor":       dev.vendor,
			"class":        dev.class,
			"device":       dev.device,
			"numa_node":    dev.numaNode,
			"sriov_numvfs": dev.numVFs,
		}
		for name, value := range files {
			if value == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(devDir, name), []byte(value+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		if dev.driver != "" {
			driverDir := filepath.Join(dir, "sys", "bus", "pci", "drivers", dev.driver)
			if err := os.MkdirAll(driverDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(driverDir, filepath.Join(devDir, "driver")); err != nil {
				t.Fatal(err)
			}
		}

		if dev.iommuGroup != "" {
			groupDir := filepath.Join(dir, "sys", "kernel", "iommu_groups", dev.iommuGroup)
			if err := os.MkdirAll(filepath.Join(groupDir, "devices"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(groupDir, filepath.Join(devDir, "iommu_group")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(devDir, filepath.Join(groupDir, "devices", dev.busID)); err != nil {
				t.Fatal(err)
			}
		}

		if dev.physfnOf != "" {
			if err := os.Symlink("../"+dev.physfnOf, filepath.Join(devDir, "physfn")); err != nil {
				t.Fatal(err)
			}
		}

		if err := os.Symlink(devDir, filepath.Join(busDir, dev.busID)); err != nil {
			t.Fatal(err)
		}
	}

	return busDir
}

func rblnVfioDevice(busID string) fakePCIDevice {
	group, _ := strconv.ParseUint(strings.NewReplacer(":", "", ".", "").Replace(busID), 16, 64)
	return fakePCIDevice{
		busID:      busID,
		vendor:     rblnPCIVendorID,
		class:      npuPCIClassID,
		device:     "0x1251",
		driver:     vfioPCIDriverName,
		iommuGroup: strconv.FormatUint(group, 10),
		numaNode:   "0",
		rootPort:   "0000:00:01.0",
	}
}

func TestEnumerateVfioDevices(t *testing.T) {
	dev := rblnVfioDevice("0000:27:00.0")
	dev.iommuGroup = "42"
	root := writeFakeSysfs(t, []fakePCIDevice{dev})

	devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}

	got := devices[0]
	if got.Name != "vfio-0000-27-00-0" {
		t.Errorf("unexpected device name %q", got.Name)
	}

	wantString := map[resourceapi.QualifiedName]string{
		deviceTypeAttributeKey:    deviceTypeVfio,
		partitionModeAttributeKey: "none",
		pciBusIDAttributeKey:      "0000:27:00.0",
		pcieRootAttributeKey:      "pci0000:00",
		"pciDeviceID":             "1251",
		"productName":             "RBLN-CA25",
	}
	for key, want := range wantString {
		attr, ok := got.Attributes[key]
		if !ok || attr.StringValue == nil {
			t.Errorf("missing string attribute %s", key)
			continue
		}
		if *attr.StringValue != want {
			t.Errorf("attribute %s = %q, want %q", key, *attr.StringValue, want)
		}
	}

	wantInt := map[resourceapi.QualifiedName]int64{
		iommuGroupAttributeKey:     42,
		numVFsAttributeKey:         0,
		numaNodeAttributeKey:       0,
		dranetNumaNodeAttributeKey: 0,
	}
	for key, want := range wantInt {
		attr, ok := got.Attributes[key]
		if !ok || attr.IntValue == nil {
			t.Errorf("missing int attribute %s", key)
			continue
		}
		if *attr.IntValue != want {
			t.Errorf("attribute %s = %d, want %d", key, *attr.IntValue, want)
		}
	}

	if attr := got.Attributes["vf"]; attr.BoolValue == nil || *attr.BoolValue {
		t.Errorf("attribute vf should be false for a physical function")
	}
}

func TestEnumerateVfioDevicesCR13ProductName(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deviceID string
		vf       bool
	}{
		{name: "PF", deviceID: "0x2130"},
		{name: "VF", deviceID: "0x2131", vf: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := rblnVfioDevice("0000:27:00.0")
			dev.device = tc.deviceID
			layout := []fakePCIDevice{dev}
			if tc.vf {
				layout[0].physfnOf = "0000:26:00.0"
				layout = append(layout, nativeDevice("0000:26:00.0", "4"))
			}
			root := writeFakeSysfs(t, layout)

			devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(devices) != 1 {
				t.Fatalf("expected 1 device, got %d", len(devices))
			}

			// ResourceClaim selectors must be able to match CR13 by productName
			// while distinguishing physical and virtual functions via vf.
			attrs := devices[0].Attributes
			if attr := attrs["productName"]; attr.StringValue == nil || *attr.StringValue != "RBLN-CR13" {
				t.Errorf("CR13 device %s must publish productName=RBLN-CR13, got %+v", tc.deviceID, attr)
			}
			if attr := attrs["vf"]; attr.BoolValue == nil || *attr.BoolValue != tc.vf {
				t.Errorf("CR13 device %s must publish vf=%t, got %+v", tc.deviceID, tc.vf, attr)
			}
		})
	}
}

func TestEnumerateVfioDevicesFiltering(t *testing.T) {
	rblnBound := rblnVfioDevice("0000:27:00.0")
	rblnBound.driver = "rebellions"

	otherVendor := rblnVfioDevice("0000:28:00.0")
	otherVendor.vendor = "0x10de"

	otherClass := rblnVfioDevice("0000:29:00.0")
	otherClass.class = "0x020000"

	unbound := rblnVfioDevice("0000:2a:00.0")
	unbound.driver = ""

	noIOMMU := rblnVfioDevice("0000:2b:00.0")
	noIOMMU.iommuGroup = ""

	wanted := rblnVfioDevice("0000:2c:00.0")

	root := writeFakeSysfs(t, []fakePCIDevice{rblnBound, otherVendor, otherClass, unbound, noIOMMU, wanted})

	devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected exactly 1 device, got %d: %v", len(devices), devices)
	}
	if devices[0].Name != "vfio-0000-2c-00-0" {
		t.Errorf("unexpected device name %q", devices[0].Name)
	}
}

func TestEnumerateVfioDevicesVirtualFunction(t *testing.T) {
	for _, count := range []string{"1", "4"} {
		t.Run("vf"+count, func(t *testing.T) {
			pf := "0000:27:00.0"
			root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, count), vfioVF("0000:27:00.1", pf)})
			devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
			if err != nil || len(devices) != 1 {
				t.Fatalf("enumeration = %v, %v", devices, err)
			}
			d := devices[0]
			if deviceBoolAttr(d, vfAttributeKey) != true || deviceStringAttr(d, partitionModeAttributeKey) != "vf"+count ||
				deviceStringAttr(d, parentPCIBusIDAttributeKey) != pf || deviceStringAttr(d, deviceTypeAttributeKey) != deviceTypeVfio {
				t.Errorf("unexpected VF attributes: %v", d.Attributes)
			}
			wantCount := int64(1)
			if count == "4" {
				wantCount = 4
			}
			if deviceIntAttr(d, numVFsAttributeKey) != wantCount {
				t.Errorf("numVFs = %v, want %d", deviceIntAttr(d, numVFsAttributeKey), wantCount)
			}
		})
	}
}

func TestEnumerateVfioDevicesExcludesUnsupportedSRIOV(t *testing.T) {
	pf := "0000:27:00.0"
	for _, count := range []string{"0", "2"} {
		t.Run("parent count "+count, func(t *testing.T) {
			root := writeFakeSysfs(t, []fakePCIDevice{nativeDevice(pf, count), vfioVF("0000:27:00.1", pf)})
			devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
			if err != nil || len(devices) != 0 {
				t.Fatalf("enumeration = %v, %v", devices, err)
			}
		})
	}
	// Even if a hosting PF is bound to vfio-pci, it is not usable compute.
	hostingPF := rblnVfioDevice(pf)
	hostingPF.numVFs = "4"
	root := writeFakeSysfs(t, []fakePCIDevice{hostingPF, vfioVF("0000:27:00.1", pf)})
	devices, _, err := enumerateVfioDevices(context.Background(), root, nil)
	if err != nil || len(devices) != 1 || devices[0].Name != vfioDeviceName("0000:27:00.1")+"-vf4" {
		t.Fatalf("hosting PF must be excluded: %v, %v", devices, err)
	}
}

func TestIsVfioDeviceAndIOMMUGroup(t *testing.T) {
	vfio := resourceapi.Device{
		Name: "vfio-0000-27-00-0",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeVfio)},
			iommuGroupAttributeKey: {IntValue: ptr.To(int64(42))},
		},
	}
	npu := resourceapi.Device{
		Name: "rbln0",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeNpu)},
		},
	}

	if !isVfioDevice(vfio) {
		t.Error("vfio device not recognized")
	}
	if isVfioDevice(npu) {
		t.Error("npu device misclassified as vfio")
	}

	group, err := deviceIOMMUGroup(vfio)
	if err != nil {
		t.Fatal(err)
	}
	if group != "42" {
		t.Errorf("iommu group = %q, want %q", group, "42")
	}

	if _, err := deviceIOMMUGroup(npu); err == nil {
		t.Error("expected error for device without iommu group")
	}
}

// Enumeration returns exclusion details; only the caller logs transitions.
func TestEnumerateVfioDevicesReturnsMissingIOMMUGroupWithoutLogging(t *testing.T) {
	buf := logtest.Capture(t, "info")

	noIOMMU := rblnVfioDevice("0000:2b:00.0")
	noIOMMU.iommuGroup = ""
	root := writeFakeSysfs(t, []fakePCIDevice{noIOMMU})

	devices, skipped, err := enumerateVfioDevices(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 0 {
		t.Fatalf("device without IOMMU group was advertised: %v", devices)
	}

	if len(skipped) != 1 || skipped[0].pciBusID != noIOMMU.busID || !strings.Contains(skipped[0].reason(), "no IOMMU group") {
		t.Fatalf("missing exclusion details: %+v", skipped)
	}
	if buf.Len() != 0 {
		t.Fatalf("enumeration must not log per-tick exclusions: %s", buf.String())
	}
}
