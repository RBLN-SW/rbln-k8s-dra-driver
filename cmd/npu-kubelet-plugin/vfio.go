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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	sysfsPCIDevicesRoot = "/sys/bus/pci/devices"

	// Same selection criteria as the vfio-manager bind script in the NPU
	// operator: Rebellions vendor with the processing-accelerator class.
	rblnPCIVendorID = "0x1eff"
	npuPCIClassID   = "0x120000"

	vfioPCIDriverName    = "vfio-pci"
	vfioDevDir           = "/dev/vfio"
	vfioContainerDevPath = "/dev/vfio/vfio"

	// Ownership applied to vfio device nodes inside the container so the
	// QEMU process in KubeVirt's virt-launcher (uid/gid 107) can open them.
	qemuUID uint32 = 107
	qemuGID uint32 = 107
)

var pciAddressRegexp = regexp.MustCompile(`^\d{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

var pciDeviceIDToProductName = map[string]string{
	"1220": "RBLN-CA22",
	"1221": "RBLN-CA22",
	"1250": "RBLN-CA25",
	"1251": "RBLN-CA25",
}

func enumerateVfioDevices(root string) ([]resourceapi.Device, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}

	var devices []resourceapi.Device
	for _, entry := range entries {
		busID := entry.Name()
		if !pciAddressRegexp.MatchString(busID) {
			continue
		}
		devPath := filepath.Join(root, busID)

		if vendor, err := readSysfsValue(devPath, "vendor"); err != nil || vendor != rblnPCIVendorID {
			continue
		}
		if class, err := readSysfsValue(devPath, "class"); err != nil || class != npuPCIClassID {
			continue
		}
		if driver, err := readSysfsLinkBase(devPath, "driver"); err != nil || driver != vfioPCIDriverName {
			continue
		}

		group, err := deviceIOMMUGroupFromSysfs(devPath)
		if err != nil {
			// A vfio device without an IOMMU group cannot be passed
			// through, so it must not be advertised.
			continue
		}

		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {
				StringValue: ptr.To(deviceTypeVfio),
			},
			pciBusIDAttributeKey: {
				StringValue: ptr.To(busID),
			},
			iommuGroupAttributeKey: {
				IntValue: ptr.To(group),
			},
		}

		if deviceID, err := readSysfsValue(devPath, "device"); err == nil {
			deviceID = strings.TrimPrefix(deviceID, "0x")
			attrs["pciDeviceID"] = resourceapi.DeviceAttribute{StringValue: ptr.To(deviceID)}
			if productName, ok := pciDeviceIDToProductName[deviceID]; ok {
				attrs["productName"] = resourceapi.DeviceAttribute{StringValue: ptr.To(productName)}
			}
		}

		if pcieRoot, err := resolvePCIERootID(root, busID); err == nil && pcieRoot != "" {
			attrs[pcieRootAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(pcieRoot)}
		}

		if numa, err := readSysfsValue(devPath, "numa_node"); err == nil {
			if v, err := strconv.ParseInt(numa, 10, 64); err == nil && v >= 0 {
				attrs[numaNodeAttributeKey] = resourceapi.DeviceAttribute{IntValue: ptr.To(v)}
			}
		}

		_, err = os.Lstat(filepath.Join(devPath, "physfn"))
		attrs["vf"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(err == nil)}

		devices = append(devices, resourceapi.Device{
			Name:       vfioDeviceName(busID),
			Attributes: attrs,
		})
	}

	return devices, nil
}

func vfioDeviceName(busID string) string {
	return "vfio-" + strings.ToLower(strings.NewReplacer(":", "-", ".", "-").Replace(busID))
}

func readSysfsValue(devPath, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(devPath, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readSysfsLinkBase(devPath, name string) (string, error) {
	target, err := os.Readlink(filepath.Join(devPath, name))
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

func deviceIOMMUGroupFromSysfs(devPath string) (int64, error) {
	base, err := readSysfsLinkBase(devPath, "iommu_group")
	if err != nil {
		return 0, err
	}
	group, err := strconv.ParseInt(base, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected iommu group %q for %s: %w", base, devPath, err)
	}
	return group, nil
}

// resolvePCIERootID resolves the PCI address of the PCIe root port a device
// hangs off of by walking the physical device path in sysfs.
func resolvePCIERootID(root, busID string) (string, error) {
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(root, busID))
	if err != nil {
		return "", err
	}
	for segment := range strings.SplitSeq(filepath.Clean(resolvedPath), string(filepath.Separator)) {
		if pciAddressRegexp.MatchString(segment) {
			return segment, nil
		}
	}
	return "", nil
}

func isVfioDevice(device resourceapi.Device) bool {
	attr, ok := device.Attributes[deviceTypeAttributeKey]
	return ok && attr.StringValue != nil && *attr.StringValue == deviceTypeVfio
}

func deviceIOMMUGroup(device resourceapi.Device) (string, error) {
	attr, ok := device.Attributes[iommuGroupAttributeKey]
	if !ok || attr.IntValue == nil {
		return "", fmt.Errorf("device %q is missing attribute %s", device.Name, iommuGroupAttributeKey)
	}
	return strconv.FormatInt(*attr.IntValue, 10), nil
}

func vfioContainerEdits(device resourceapi.Device) (*cdispec.ContainerEdits, error) {
	group, err := deviceIOMMUGroup(device)
	if err != nil {
		return nil, err
	}

	edits := &cdispec.ContainerEdits{}
	for _, path := range []string{vfioContainerDevPath, filepath.Join(vfioDevDir, group)} {
		if _, err := waitForDeviceNode(path); err != nil {
			return nil, fmt.Errorf("vfio device node %q: %w", path, err)
		}
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{
			Path:        path,
			HostPath:    path,
			Permissions: "mrw",
			UID:         ptr.To(qemuUID),
			GID:         ptr.To(qemuGID),
		})
	}
	// virt-launcher resolves the passthrough PCI address from KEP-5304
	// metadata files (see kubevirt_metadata.go). KubeVirt does not mount
	// that directory itself, so expose it to the consuming container here.
	edits.Mounts = append(edits.Mounts, &cdispec.Mount{
		HostPath:      kubevirtMetadataBasePath,
		ContainerPath: kubevirtMetadataBasePath,
		Options:       []string{"ro", "bind"},
	})
	return edits, nil
}
