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
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	host "github.com/rbln-sw/rblnlib-go/pkg/host/v1"
	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	// Same selection criteria as the vfio-manager bind script in the NPU
	// operator: Rebellions vendor with the processing-accelerator class.

	vfioPCIDriverName = system.VFIODriver

	// Ownership applied to vfio device nodes inside the container so the
	// QEMU process in KubeVirt's virt-launcher (uid/gid 107) can open them.
	qemuUID uint32 = 107
	qemuGID uint32 = 107
)

// enumerateVfioDevices applies DRA's allocation policy to library observations.
func enumerateVfioDevices(ctx context.Context, root string, known map[string]bool) ([]resourceapi.Device, []skippedDevice, error) {
	snapshot, err := systemInventory(ctx, root, known)
	if err != nil {
		return nil, nil, err
	}
	products, err := host.BuiltinPCIIDs()
	if err != nil {
		return nil, nil, err
	}
	var devices []resourceapi.Device
	var skipped []skippedDevice
	for _, d := range snapshot.Devices {
		busID := d.PCIAddress
		if !d.DriverKnown {
			return nil, nil, system.IssuesError(d.Issues)
		}
		if d.Driver != vfioPCIDriverName {
			continue
		}
		name := vfioDeviceName(busID)
		sriov, err := probeSRIOV(root, busID)
		if err != nil {
			if !errors.Is(err, errUnsupportedDevice) {
				return nil, nil, err
			}
			skipped = append(skipped, skippedDevice{name: name, pciBusID: busID, err: err})
			continue
		}
		if sriov.isHostingPF() {
			skipped = append(skipped, skippedDevice{name: name, pciBusID: busID, sriov: sriov})
			continue
		}
		name = sriov.deviceName(name)
		group, err := isolatedIOMMUGroupContext(ctx, filepath.Join(root, busID), busID)
		if err != nil {
			if !errors.Is(err, errUnsupportedDevice) {
				return nil, nil, err
			}
			skipped = append(skipped, skippedDevice{name: name, pciBusID: busID, err: err})
			continue
		}
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {StringValue: ptr.To(deviceTypeVfio)},
			pciBusIDAttributeKey:   {StringValue: ptr.To(busID)},
			iommuGroupAttributeKey: {IntValue: ptr.To(group)},
			"pciDeviceID":          {StringValue: ptr.To(fmt.Sprintf("%04x", d.DeviceID))},
		}
		if product := products.Lookup(d.DeviceID); product != "" {
			attrs["productName"] = resourceapi.DeviceAttribute{StringValue: ptr.To(strings.Fields(product)[0])}
		}
		if d.Topology.NUMANode != nil {
			setNumaNodeAttr(ctx, attrs, name, strconv.Itoa(*d.Topology.NUMANode))
		}
		setPCIERootAttr(ctx, attrs, name, root, busID)
		sriov.setAttributes(attrs)
		devices = append(devices, resourceapi.Device{Name: name, Attributes: attrs})
	}
	return devices, skipped, nil
}

func vfioDeviceName(busID string) string {
	return "vfio-" + strings.ToLower(strings.NewReplacer(":", "-", ".", "-").Replace(busID))
}

// DRA allocates one function at a time and therefore requires an exclusive group.
func isolatedIOMMUGroup(devPath, busID string) (int64, error) {
	return isolatedIOMMUGroupContext(context.Background(), devPath, busID)
}

func isolatedIOMMUGroupContext(ctx context.Context, devPath, busID string) (int64, error) {
	c, err := systemHost(filepath.Dir(devPath))
	if err != nil {
		return 0, err
	}
	group, members, err := c.IOMMUGroup(ctx, busID)
	if errors.Is(err, system.ErrUnsupported) {
		return 0, fmt.Errorf("%w: no IOMMU group for %s", errUnsupportedDevice, busID)
	}
	if err != nil {
		return 0, err
	}
	if len(members) != 1 || !strings.EqualFold(members[0], busID) {
		return 0, fmt.Errorf("%w: IOMMU group %d is not exclusive to %s", errUnsupportedDevice, group, busID)
	}
	return int64(group), nil
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

func vfioContainerEdits(ctx context.Context, device resourceapi.Device, metadataMounts []*cdispec.Mount) (*cdispec.ContainerEdits, error) {
	group, err := deviceIOMMUGroup(device)
	if err != nil {
		return nil, err
	}

	edits := &cdispec.ContainerEdits{}
	id, err := strconv.ParseUint(group, 10, 32)
	if err != nil {
		return nil, err
	}
	machine, err := host.New(host.Options{})
	if err != nil {
		return nil, err
	}
	nodes, err := machine.VFIONodes(ctx, uint32(id))
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		edits.DeviceNodes = append(edits.DeviceNodes, &cdispec.DeviceNode{Path: node.Path, HostPath: node.HostPath, Permissions: "mrw", UID: ptr.To(qemuUID), GID: ptr.To(qemuGID)})
	}

	// Mount only this request's files. The host paths are claim-UID scoped;
	// each container still sees the standard name/alias-based lookup paths.
	edits.Mounts = append(edits.Mounts, metadataMounts...)
	return edits, nil
}
