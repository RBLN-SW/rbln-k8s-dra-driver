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
	"os"
	"path/filepath"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
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
	physfn     bool
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
			"vendor":    dev.vendor,
			"class":     dev.class,
			"device":    dev.device,
			"numa_node": dev.numaNode,
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
			if err := os.MkdirAll(groupDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(groupDir, filepath.Join(devDir, "iommu_group")); err != nil {
				t.Fatal(err)
			}
		}

		if dev.physfn {
			if err := os.Symlink(devDir, filepath.Join(devDir, "physfn")); err != nil {
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
	return fakePCIDevice{
		busID:      busID,
		vendor:     rblnPCIVendorID,
		class:      npuPCIClassID,
		device:     "0x1251",
		driver:     vfioPCIDriverName,
		iommuGroup: "42",
		numaNode:   "0",
		rootPort:   "0000:00:01.0",
	}
}

func TestEnumerateVfioDevices(t *testing.T) {
	dev := rblnVfioDevice("0000:27:00.0")
	root := writeFakeSysfs(t, []fakePCIDevice{dev})

	devices, err := enumerateVfioDevices(root)
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
		deviceTypeAttributeKey: deviceTypeVfio,
		pciBusIDAttributeKey:   "0000:27:00.0",
		pcieRootAttributeKey:   "pci0000:00",
		"pciDeviceID":          "1251",
		"productName":          "RBLN-CA25",
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

	devices, err := enumerateVfioDevices(root)
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
	vf := rblnVfioDevice("0000:27:00.1")
	vf.physfn = true
	root := writeFakeSysfs(t, []fakePCIDevice{vf})

	devices, err := enumerateVfioDevices(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if attr := devices[0].Attributes["vf"]; attr.BoolValue == nil || !*attr.BoolValue {
		t.Errorf("attribute vf should be true for a virtual function")
	}
}

func TestRescanVfioDevices(t *testing.T) {
	root := writeFakeSysfs(t, []fakePCIDevice{rblnVfioDevice("0000:27:00.0")})

	npuDevice := resourceapi.Device{
		Name: "rbln0",
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeNpu)},
		},
	}

	state := &DeviceState{
		nodeName:            "node-1",
		npuDevices:          []resourceapi.Device{npuDevice},
		sysfsPCIDevicesRoot: root,
	}
	state.rebuildResources()

	if len(state.allocatable) != 1 {
		t.Fatalf("expected only the npu device before rescan, got %d", len(state.allocatable))
	}

	resources, changed, err := state.RescanVfioDevices()
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected first rescan to report a change")
	}
	if len(state.allocatable) != 2 {
		t.Fatalf("expected npu + vfio devices, got %d", len(state.allocatable))
	}
	if _, ok := state.allocatable["vfio-0000-27-00-0"]; !ok {
		t.Error("vfio device missing from allocatable")
	}
	if _, ok := state.allocatable["rbln0"]; !ok {
		t.Error("npu device missing from allocatable")
	}
	if got := len(resources.Pools["node-1"].Slices[0].Devices); got != 2 {
		t.Errorf("expected 2 devices in published slice, got %d", got)
	}

	_, changed, err = state.RescanVfioDevices()
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("expected second rescan to report no change")
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
