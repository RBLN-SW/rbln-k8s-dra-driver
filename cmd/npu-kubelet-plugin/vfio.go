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
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
)

const (
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

var pciDeviceIDToProductName = map[string]string{
	"1220": "RBLN-CA22",
	"1221": "RBLN-CA22",
	"1250": "RBLN-CA25",
	"1251": "RBLN-CA25",
}

// enumerateVfioDevices lists the Rebellions NPUs bound to vfio-pci under the
// sysfs PCI devices root. It runs at startup and on every rescan tick, so it
// only logs conditions that drop a device; per-device detail is logged by the
// callers when the set is first seen or changes.
func enumerateVfioDevices(ctx context.Context, root string) ([]resourceapi.Device, error) {
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

		name := vfioDeviceName(busID)

		group, err := deviceIOMMUGroupFromSysfs(devPath)
		if err != nil {
			// A vfio device without an IOMMU group cannot be passed through,
			// so it must not be advertised. Silently dropping it would leave a
			// VM Pending with no record naming the card it wanted.
			logging.FromContext(ctx).Warn("Skipping vfio-pci NPU without an IOMMU group",
				"device", name, "pciBusID", busID, "err", err,
				"impact", "device is not published for passthrough; check that the IOMMU is enabled")
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

		setPCIERootAttr(ctx, attrs, name, root, busID)

		if numa, err := readSysfsValue(devPath, "numa_node"); err == nil {
			setNumaNodeAttr(ctx, attrs, name, numa)
		}

		_, err = os.Lstat(filepath.Join(devPath, "physfn"))
		attrs["vf"] = resourceapi.DeviceAttribute{BoolValue: ptr.To(err == nil)}

		devices = append(devices, resourceapi.Device{
			Name:       name,
			Attributes: attrs,
		})
	}

	return devices, nil
}

func vfioDeviceName(busID string) string {
	return "vfio-" + strings.ToLower(strings.NewReplacer(":", "-", ".", "-").Replace(busID))
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

func vfioContainerEdits(ctx context.Context, device resourceapi.Device, metadataFiles []string) (*cdispec.ContainerEdits, error) {
	group, err := deviceIOMMUGroup(device)
	if err != nil {
		return nil, err
	}

	edits := &cdispec.ContainerEdits{}
	for _, path := range []string{vfioContainerDevPath, filepath.Join(vfioDevDir, group)} {
		if _, err := waitForDeviceNode(ctx, path); err != nil {
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
	// those files itself, so expose them to the consuming container
	// here. Only this claim's files are mounted — the base path holds
	// every claim's metadata on the node, which must not leak across
	// tenants. The container paths must mirror the host paths because
	// KubeVirt assembles the full {base}/{subdir}/{claim}/{request} path
	// when globbing.
	for _, file := range metadataFiles {
		edits.Mounts = append(edits.Mounts, &cdispec.Mount{
			HostPath:      file,
			ContainerPath: file,
			Options:       []string{"ro", "bind"},
		})
	}
	return edits, nil
}
