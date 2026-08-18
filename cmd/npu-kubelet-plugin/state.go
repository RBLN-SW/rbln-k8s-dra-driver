/*
 * Copyright 2023 The Kubernetes Authors.
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
	"os"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/resourceslice"
	"k8s.io/klog/v2"
	drapbv1 "k8s.io/kubelet/pkg/apis/dra/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager"
	"k8s.io/utils/ptr"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	"github.com/rbln-sw/rblnlib-go/pkg/device"
	"github.com/rbln-sw/rblnlib-go/pkg/rsdgroup"
)

type AllocatableDevices map[string]resourceapi.Device
type PreparedClaims map[string]PreparedDevices

const (
	deviceNodePollTimeout  = 5 * time.Second
	deviceNodePollInterval = 100 * time.Millisecond
	pciBusIDAttributeKey   = resourceapi.QualifiedName("resource.kubernetes.io/pciBusID")
	pcieRootAttributeKey   = resourceapi.QualifiedName("resource.kubernetes.io/pcieRoot")
	numaNodeAttributeKey   = resourceapi.QualifiedName("resource.kubernetes.io/numaNode")
	deviceTypeAttributeKey = resourceapi.QualifiedName("type")
	iommuGroupAttributeKey = resourceapi.QualifiedName("iommuGroup")

	deviceTypeNpu  = "npu"
	deviceTypeVfio = "vfio"
)

type DeviceState struct {
	sync.Mutex
	driverName        string
	nodeName          string
	cdi               *CDIHandler
	driverResources   resourceslice.DriverResources
	allocatable       AllocatableDevices
	checkpointManager checkpointmanager.CheckpointManager
	rsdGroupFn        func([]string) string

	npuDevices          []resourceapi.Device
	vfioDevices         []resourceapi.Device
	sysfsPCIDevicesRoot string
}

func NewDeviceState(ctx context.Context, config *Config) (*DeviceState, error) {
	logger := klog.FromContext(ctx)

	npuDevices, npuErr := enumerateNpuDevices(ctx)
	vfioDevices, vfioErr := enumerateVfioDevices(sysfsPCIDevicesRoot)
	if vfioErr != nil {
		logger.Error(vfioErr, "Unable to enumerate vfio-pci NPU devices")
		vfioDevices = nil
	}
	if npuErr != nil {
		if len(vfioDevices) == 0 {
			return nil, fmt.Errorf("error enumerating all possible devices: %v", npuErr)
		}
		logger.Info("rbln-smi enumeration unavailable, continuing with vfio-pci devices only", "reason", npuErr)
		npuDevices = nil
	}

	cdi, err := NewCDIHandler(config.flags.cdiRoot, config.flags.driverName, "npu")
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	err = cdi.CreateCommonSpecFile()
	if err != nil {
		if len(npuDevices) > 0 {
			return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
		}
		logger.Info("Skipping common CDI spec file, RBLN runtime spec unavailable", "reason", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	state := &DeviceState{
		driverName:          config.flags.driverName,
		nodeName:            config.flags.nodeName,
		cdi:                 cdi,
		checkpointManager:   checkpointManager,
		rsdGroupFn:          rsdgroup.RecreateRsdGroup,
		npuDevices:          npuDevices,
		vfioDevices:         vfioDevices,
		sysfsPCIDevicesRoot: sysfsPCIDevicesRoot,
	}
	state.rebuildResources()

	checkpoints, err := state.checkpointManager.ListCheckpoints()
	if err != nil {
		return nil, fmt.Errorf("unable to list checkpoints: %v", err)
	}

	if slices.Contains(checkpoints, DriverPluginCheckpointFile) {
		return state, nil
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return state, nil
}

// Callers must hold the lock unless the state is not shared yet.
func (s *DeviceState) rebuildResources() {
	devices := slices.Concat(s.npuDevices, s.vfioDevices)

	allocatable := make(AllocatableDevices, len(devices))
	for _, d := range devices {
		allocatable[d.Name] = d
	}

	s.allocatable = allocatable
	s.driverResources = resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			s.nodeName: {
				Slices: []resourceslice.Slice{
					{
						Devices: devices,
					},
				},
			},
		},
	}
}

func (s *DeviceState) DriverResources() resourceslice.DriverResources {
	s.Lock()
	defer s.Unlock()
	return s.driverResources
}

func (s *DeviceState) RescanVfioDevices() (resourceslice.DriverResources, bool, error) {
	vfioDevices, err := enumerateVfioDevices(s.sysfsPCIDevicesRoot)
	if err != nil {
		return resourceslice.DriverResources{}, false, err
	}

	s.Lock()
	defer s.Unlock()

	// The vfio devices carry no resource.Quantity values, so
	// reflect.DeepEqual is a safe comparison here.
	if reflect.DeepEqual(vfioDevices, s.vfioDevices) {
		return s.driverResources, false, nil
	}

	s.vfioDevices = vfioDevices
	s.rebuildResources()
	return s.driverResources, true, nil
}

func (s *DeviceState) Prepare(claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	s.Lock()
	defer s.Unlock()

	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] != nil {
		return preparedClaims[claimUID].
			GetDevices(), nil
	}

	preparedDevices, err := s.prepareDevices(claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %v", err)
	}

	if err = s.cdi.CreateClaimSpecFile(claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %v", err)
	}

	if s.claimHasVfioDevice(claim) {
		dirs, err := writeKubeVirtMetadata(kubevirtMetadataBasePath, claim, s.driverName, s.allocatable)
		if err != nil {
			return nil, fmt.Errorf("unable to write KubeVirt device metadata: %v", err)
		}
		if checkpoint.V1.KubeVirtMetadataDirs == nil {
			checkpoint.V1.KubeVirtMetadataDirs = make(map[string][]string)
		}
		checkpoint.V1.KubeVirtMetadataDirs[claimUID] = dirs
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

// Callers must hold the lock.
func (s *DeviceState) claimHasVfioDevice(claim *resourceapi.ResourceClaim) bool {
	if claim.Status.Allocation == nil {
		return false
	}
	for _, result := range claim.Status.Allocation.Devices.Results {
		if result.Driver != s.driverName {
			continue
		}
		if device, ok := s.allocatable[result.Device]; ok && isVfioDevice(device) {
			return true
		}
	}
	return false
}

func (s *DeviceState) Unprepare(claimUID string) error {
	s.Lock()
	defer s.Unlock()

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync from checkpoint: %v", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		return nil
	}

	if err := s.unprepareDevices(claimUID, preparedClaims[claimUID]); err != nil {
		return fmt.Errorf("unprepare failed: %v", err)
	}

	err := s.cdi.DeleteClaimSpecFile(claimUID)
	if err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %v", err)
	}

	if dirs := checkpoint.V1.KubeVirtMetadataDirs[claimUID]; len(dirs) > 0 {
		if err := removeKubeVirtMetadata(dirs); err != nil {
			return fmt.Errorf("unable to remove KubeVirt device metadata: %v", err)
		}
		delete(checkpoint.V1.KubeVirtMetadataDirs, claimUID)
	}

	delete(preparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return nil
}

func (s *DeviceState) prepareDevices(claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
	if claim.Status.Allocation == nil {
		return nil, fmt.Errorf("claim not yet allocated")
	}

	var results []*resourceapi.DeviceRequestAllocationResult
	for i := range claim.Status.Allocation.Devices.Results {
		result := &claim.Status.Allocation.Devices.Results[i]
		if result.Driver != s.driverName {
			continue
		}
		if _, exists := s.allocatable[result.Device]; !exists {
			return nil, fmt.Errorf("requested device is not allocatable: %v", result.Device)
		}
		results = append(results, result)
	}

	// RSD groups only exist for devices driven by the rebellions kernel driver.
	var npuBusIDs []string
	for _, result := range results {
		device := s.allocatable[result.Device]
		if isVfioDevice(device) {
			continue
		}
		busID, err := devicePCIBusID(device)
		if err != nil {
			return nil, err
		}
		npuBusIDs = append(npuBusIDs, busID)
	}

	hostRsdPath := ""
	if len(npuBusIDs) > 0 {
		hostRsdPath = s.rsdGroupFn(npuBusIDs)
	}

	rdsNodes, err := s.cdi.getRDSDeviceNodes()
	if err != nil {
		klog.Warningf("reading RDS CDI spec failed for claim %s, continuing without RDS: %v", claim.UID, err)
	}

	var preparedDevices PreparedDevices
	rdsInjected := false
	for _, result := range results {
		var edits *cdispec.ContainerEdits
		var err error

		vfio := isVfioDevice(s.allocatable[result.Device])
		if vfio {
			edits, err = vfioContainerEdits(s.allocatable[result.Device])
		} else {
			edits, err = s.applyConfig(result.Device, hostRsdPath)
		}
		if err != nil {
			return nil, err
		}

		// RDS is a claim-scoped shared device (like /dev/rsd0); inject it once,
		// and only for devices driven by the rebellions kernel driver.
		if !vfio && !rdsInjected {
			edits.DeviceNodes = append(edits.DeviceNodes, rdsNodes...)
			rdsInjected = true
		}

		device := &PreparedDevice{
			Device: drapbv1.Device{
				RequestNames: []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CdiDeviceIds: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}, !vfio),
			},
			ContainerEdits: &cdiapi.ContainerEdits{ContainerEdits: edits},
		}
		preparedDevices = append(preparedDevices, device)
	}

	return preparedDevices, nil
}

func (s *DeviceState) unprepareDevices(claimUID string, devices PreparedDevices) error {
	return nil
}

func newDeviceNode(containerPath, hostPath string) (*cdispec.DeviceNode, error) {
	if _, err := waitForDeviceNode(hostPath); err != nil {
		return nil, fmt.Errorf("stat device %q: %w", hostPath, err)
	}
	return &cdispec.DeviceNode{
		Path:     containerPath,
		HostPath: hostPath,
	}, nil
}

func waitForDeviceNode(hostPath string) (os.FileInfo, error) {
	deadline := time.Now().Add(deviceNodePollTimeout)
	for {
		fi, err := os.Stat(hostPath)
		if err == nil {
			return fi, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(deviceNodePollInterval)
	}
}

func (s *DeviceState) applyConfig(deviceName, hostRsdPath string) (*cdispec.ContainerEdits, error) {
	edits := &cdispec.ContainerEdits{}
	if hostRsdPath != "" {
		rsdNode, err := newDeviceNode("/dev/rsd0", hostRsdPath)
		if err != nil {
			return nil, fmt.Errorf("rsd device node: %w", err)
		}
		edits.DeviceNodes = append(edits.DeviceNodes, rsdNode)
	}
	allocatable, ok := s.allocatable[deviceName]
	if !ok {
		return nil, fmt.Errorf("allocatable device %q not found", deviceName)
	}
	rblnPath := fmt.Sprintf("/dev/%s", allocatable.Name)
	rblnNode, err := newDeviceNode(rblnPath, rblnPath)
	if err != nil {
		return nil, fmt.Errorf("rbln device node: %w", err)
	}
	edits.DeviceNodes = append(edits.DeviceNodes, rblnNode)
	return edits, nil
}

func devicePCIBusID(device resourceapi.Device) (string, error) {
	attr, ok := device.Attributes[pciBusIDAttributeKey]
	if !ok || attr.StringValue == nil || *attr.StringValue == "" {
		return "", fmt.Errorf("allocatable device %q is missing attribute %s", device.Name, pciBusIDAttributeKey)
	}
	return *attr.StringValue, nil
}

func enumerateNpuDevices(ctx context.Context) ([]resourceapi.Device, error) {
	devs, err := device.GetDevices(ctx)
	if err != nil {
		return nil, err
	}

	var devices []resourceapi.Device
	for _, d := range devs {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			deviceTypeAttributeKey: {
				StringValue: ptr.To(deviceTypeNpu),
			},
			"productName": {
				StringValue: ptr.To(d.ProductName),
			},
			"sid": {
				StringValue: ptr.To(d.SID),
			},
			"uuid": {
				StringValue: ptr.To(d.UUID),
			},
			"pciDeviceID": {
				StringValue: ptr.To(d.PCIDeviceID),
			},
			pciBusIDAttributeKey: {
				StringValue: ptr.To(d.PCIBusID),
			},
			pcieRootAttributeKey: {
				StringValue: ptr.To(d.PCIERootID),
			},
			"pciLinkSpeed": {
				StringValue: ptr.To(d.PCILinkSpeed),
			},
			"pciLinkWidth": {
				StringValue: ptr.To(d.PCILinkWidth),
			},
			"firmwareVersion": {
				StringValue: ptr.To(d.FirmwareVersion),
			},
			"driverVersion": {
				StringValue: ptr.To(d.KMDVersion),
			},
		}
		if d.PCINumaNode != "" {
			if v, err := strconv.ParseInt(d.PCINumaNode, 10, 64); err == nil {
				attrs[numaNodeAttributeKey] = resourceapi.DeviceAttribute{IntValue: ptr.To(v)}
			}
		}
		device := resourceapi.Device{
			Name:       d.Name,
			Attributes: attrs,
		}
		if d.MemoryTotalBytes > 0 {
			device.Capacity = map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
				"memory": {
					Value: *resource.NewQuantity(d.MemoryTotalBytes, resource.BinarySI),
				},
			}
		}
		devices = append(devices, device)
	}

	return devices, nil
}
