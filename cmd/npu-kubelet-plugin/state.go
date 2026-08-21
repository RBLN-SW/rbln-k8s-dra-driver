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

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
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
		return nil, fmt.Errorf("error enumerating all possible devices: %w", err)
	}

	cdi, err := NewCDIHandler(config.flags.cdiRoot, config.flags.driverName, "npu")
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %w", err)
	}

	err = cdi.CreateCommonSpecFile(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for common edits: %w", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %w", err)
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
		return nil, fmt.Errorf("unable to list checkpoints: %w", err)
	}

	if slices.Contains(checkpoints, DriverPluginCheckpointFile) {
		return state, nil
	}

	checkpoint := newCheckpoint()
	if err := state.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %w", err)
	}

	return state, nil
}

func (s *DeviceState) Prepare(ctx context.Context, claim *resourceapi.ResourceClaim) ([]*drapbv1.Device, error) {
	s.Lock()
	defer s.Unlock()

	logger := logging.FromContext(ctx)
	claimUID := string(claim.UID)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync from checkpoint: %w", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] != nil {
		// Distinguishing a checkpoint replay from a fresh prepare is what tells
		// an operator whether a restart reused state or rebuilt it.
		logger.Debug("Claim already prepared, reusing checkpointed devices",
			"deviceCount", len(preparedClaims[claimUID]))
		return preparedClaims[claimUID].
			GetDevices(), nil
	}

	preparedDevices, err := s.prepareDevices(ctx, claim)
	if err != nil {
		return nil, fmt.Errorf("prepare failed: %w", err)
	}

	if err = s.cdi.CreateClaimSpecFile(ctx, claimUID, preparedDevices); err != nil {
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %w", err)
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return nil, fmt.Errorf("unable to sync to checkpoint: %w", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

func (s *DeviceState) Unprepare(ctx context.Context, claimUID string) error {
	s.Lock()
	defer s.Unlock()

	logger := logging.FromContext(ctx)

	checkpoint := newCheckpoint()
	if err := s.checkpointManager.GetCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync from checkpoint: %w", err)
	}
	preparedClaims := checkpoint.V1.PreparedClaims

	if preparedClaims[claimUID] == nil {
		// A legitimate no-op, but a silent one leaves an operator chasing
		// leaked device nodes with no evidence either way.
		logger.Debug("No prepared state for claim, nothing to unprepare")
		return nil
	}

	// Captured before the delete below, so the record can name what was torn
	// down. Prepare logs the device names it injected; without the same names
	// here an operator cannot reconcile the two halves of a claim's lifecycle.
	deviceNames := preparedClaims[claimUID].GetDeviceNames()

	if err := s.unprepareDevices(claimUID, preparedClaims[claimUID]); err != nil {
		return fmt.Errorf("unprepare failed: %w", err)
	}

	err := s.cdi.DeleteClaimSpecFile(ctx, claimUID)
	if err != nil {
		return fmt.Errorf("unable to delete CDI spec file for claim: %w", err)
	}

	delete(preparedClaims, claimUID)
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		return fmt.Errorf("unable to sync to checkpoint: %w", err)
	}

	logger.Info("Unprepared devices for claim", "devices", deviceNames)
	return nil
}

func (s *DeviceState) prepareDevices(ctx context.Context, claim *resourceapi.ResourceClaim) (PreparedDevices, error) {
	logger := logging.FromContext(ctx)

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
		if hostRsdPath == "" {
			// applyConfig skips the shared node when the path is empty, so the
			// container starts with only its own NPUs, the pod goes Ready, and
			// peer communication is broken with nothing else to show for it.
			// This record is the only signal that the pod is quietly degraded;
			// "impact" states that in the record because the message alone
			// reads like a transient failure the caller retried.
			logger.Error("RSD group creation returned no device path",
				"busIDs", busIDs,
				"impact", "containers get no /dev/rsd0; multi-NPU peer communication disabled")
		} else {
			logger.Info("Created RSD group device", "hostRsdPath", hostRsdPath, "busIDs", busIDs)
		}
	}

	var preparedDevices PreparedDevices
	for _, result := range results {
		edits, err := s.applyConfig(ctx, result.Device, hostRsdPath)
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

func newDeviceNode(ctx context.Context, containerPath, hostPath string) (*cdispec.DeviceNode, error) {
	if _, err := waitForDeviceNode(ctx, hostPath); err != nil {
		return nil, fmt.Errorf("stat device %q: %w", hostPath, err)
	}
	return &cdispec.DeviceNode{
		Path:     containerPath,
		HostPath: hostPath,
	}, nil
}

// waitForDeviceNode polls because a freshly created RSD group node can lag the
// call that created it. The wait is logged so a multi-second stall in prepare is
// attributable; the timeout error itself is not logged here, it is returned and
// logged once at the driver boundary with the claim attached.
func waitForDeviceNode(ctx context.Context, hostPath string) (os.FileInfo, error) {
	deadline := time.Now().Add(deviceNodePollTimeout)
	started := time.Now()
	waited := false
	for {
		fi, err := os.Stat(hostPath)
		if err == nil {
			if waited {
				logging.FromContext(ctx).Debug("Device node appeared",
					"hostPath", hostPath, "waited", time.Since(started).String())
			}
			return fi, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		if !waited {
			waited = true
			logging.FromContext(ctx).Debug("Waiting for device node to appear",
				"hostPath", hostPath, "timeout", deviceNodePollTimeout.String())
		}
		time.Sleep(deviceNodePollInterval)
	}
}

func (s *DeviceState) applyConfig(ctx context.Context, deviceName, hostRsdPath string) (*cdispec.ContainerEdits, error) {
	edits := &cdispec.ContainerEdits{}
	if hostRsdPath != "" {
		rsdNode, err := newDeviceNode(ctx, "/dev/rsd0", hostRsdPath)
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
	rblnNode, err := newDeviceNode(ctx, rblnPath, rblnPath)
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

// setNumaNodeAttr adds the numaNode attribute for a device, or reports why it
// could not. Dropping it silently degrades topology-aware allocation with no
// way to tell from the outside that the device lost its NUMA affinity.
func setNumaNodeAttr(ctx context.Context, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, deviceName, numaNode string) {
	if numaNode == "" {
		return
	}
	v, err := strconv.ParseInt(numaNode, 10, 64)
	if err != nil {
		logging.FromContext(ctx).Warn("Ignoring unparseable NUMA node",
			"device", deviceName, "numaNode", numaNode, "err", err,
			"impact", "device published without a numaNode attribute")
		return
	}
	attrs[numaNodeAttributeKey] = resourceapi.DeviceAttribute{IntValue: ptr.To(v)}
}

func enumerateNpuDevices(ctx context.Context, nodeName string) (resourceslice.DriverResources, AllocatableDevices, error) {
	logger := logging.FromContext(ctx)

	devs, err := device.GetDevices(ctx)
	if err != nil {
		return resourceslice.DriverResources{}, nil, err
	}
	if len(devs) == 0 {
		// The DaemonSet only lands on nodes labelled as having NPUs, so zero
		// devices means the label, the kernel driver and reality disagree.
		logger.Warn("No NPU devices found on this node",
			"impact", "the published ResourceSlice will be empty and no pod can be scheduled here")
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
		setNumaNodeAttr(ctx, attrs, d.Name, d.PCINumaNode)
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
		// Per-device detail is what answers "the slice shows N devices but the
		// node has M".
		logger.Debug("Enumerated NPU device",
			"device", d.Name, "productName", d.ProductName, "uuid", d.UUID,
			"pciBusID", d.PCIBusID, "pcieRoot", d.PCIERootID, "numaNode", d.PCINumaNode,
			"firmwareVersion", d.FirmwareVersion, "driverVersion", d.KMDVersion,
			"memoryTotalBytes", d.MemoryTotalBytes)

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
