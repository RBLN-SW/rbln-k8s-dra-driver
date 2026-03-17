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
	"slices"
	"strconv"
	"sync"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/dynamic-resource-allocation/resourceslice"
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
)

type DeviceState struct {
	sync.Mutex
	driverName        string
	cdi               *CDIHandler
	driverResources   resourceslice.DriverResources
	allocatable       AllocatableDevices
	checkpointManager checkpointmanager.CheckpointManager
	rsdGroupFn        func([]string) string
}

func NewDeviceState(ctx context.Context, config *Config) (*DeviceState, error) {
	driverResources, allocatable, err := enumerateNpuDevices(ctx, config.flags.nodeName)
	if err != nil {
		return nil, fmt.Errorf("error enumerating all possible devices: %v", err)
	}

	cdi, err := NewCDIHandler(config.flags.cdiRoot, config.flags.driverName, "npu")
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %v", err)
	}

	err = cdi.CreateCommonSpecFile()
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %v", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %v", err)
	}

	state := &DeviceState{
		driverName:        config.flags.driverName,
		cdi:               cdi,
		driverResources:   driverResources,
		allocatable:       allocatable,
		checkpointManager: checkpointManager,
		rsdGroupFn:        rsdgroup.RecreateRsdGroup,
	}

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

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %v", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
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

	busIDs, err := s.getPCIBusIDs(results)
	if err != nil {
		return nil, err
	}

	hostRsdPath := ""
	if len(busIDs) > 0 {
		hostRsdPath = s.rsdGroupFn(busIDs)
	}

	var preparedDevices PreparedDevices
	for _, result := range results {
		edits, err := s.applyConfig(result.Device, hostRsdPath)
		if err != nil {
			return nil, err
		}
		device := &PreparedDevice{
			Device: drapbv1.Device{
				RequestNames: []string{result.Request},
				PoolName:     result.Pool,
				DeviceName:   result.Device,
				CdiDeviceIds: s.cdi.GetClaimDevices(string(claim.UID), []string{result.Device}),
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

func (s *DeviceState) getPCIBusIDs(results []*resourceapi.DeviceRequestAllocationResult) ([]string, error) {
	busIDs := make([]string, 0, len(results))
	for _, result := range results {
		device := s.allocatable[result.Device]
		attr, ok := device.Attributes[pciBusIDAttributeKey]
		if !ok || attr.StringValue == nil || *attr.StringValue == "" {
			return nil, fmt.Errorf("allocatable device %q is missing attribute %s", result.Device, pciBusIDAttributeKey)
		}
		busIDs = append(busIDs, *attr.StringValue)
	}
	return busIDs, nil
}

func enumerateNpuDevices(ctx context.Context, nodeName string) (resourceslice.DriverResources, AllocatableDevices, error) {
	devs, err := device.GetDevices(ctx)
	if err != nil {
		return resourceslice.DriverResources{}, nil, err
	}

	allocatable := make(AllocatableDevices)
	var devices []resourceapi.Device
	for _, d := range devs {
		attrs := map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			"type": {
				StringValue: ptr.To("npu"),
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
		allocatable[d.Name] = device
	}

	driverResources := resourceslice.DriverResources{
		Pools: map[string]resourceslice.Pool{
			nodeName: {
				Slices: []resourceslice.Slice{
					{
						Devices: devices,
					},
				},
			},
		},
	}

	return driverResources, allocatable, nil
}
