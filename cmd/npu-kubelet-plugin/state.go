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
	// dranetNumaNodeAttributeKey mirrors numaNodeAttributeKey under the key
	// DraNet and dra-driver-cpu publish, so one claim can matchAttribute
	// NPUs with NICs/CPUs across drivers.
	dranetNumaNodeAttributeKey = resourceapi.QualifiedName("dra.net/numaNode")
	deviceTypeAttributeKey     = resourceapi.QualifiedName("type")
	iommuGroupAttributeKey     = resourceapi.QualifiedName("iommuGroup")

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
	logger := logging.FromContext(ctx)

	npuDevices, npuErr := enumerateNpuDevices(ctx, sysfsPCIDevicesRoot)
	vfioDevices, vfioErr := enumerateVfioDevices(ctx, sysfsPCIDevicesRoot)
	if vfioErr != nil {
		// Not fatal: a container-only node is unaffected and the periodic
		// rescan retries. Error rather than warn because on a passthrough
		// node this is the record that explains why no VM can be scheduled.
		logger.Error("Failed to enumerate vfio-pci NPU devices", "err", vfioErr,
			"impact", "no passthrough device is published until a rescan succeeds")
		vfioDevices = nil
	}
	// Nodes are converted to vfio as a whole (vfio-manager binds --all), so
	// "rbln-smi failed but vfio devices exist" can only mean a genuine
	// vfio-only node and continuing without npu devices is correct. If
	// per-card binding ever makes mixed nodes possible, this branch becomes
	// unsafe: a transient rbln-smi failure (e.g. the plugin racing the
	// driver container at boot) would permanently hide every container-mode
	// NPU, because npu enumeration runs only once and the periodic rescan
	// covers vfio devices only. Re-enumerate npu devices in the rescan loop
	// before allowing mixed nodes.
	if npuErr != nil {
		if len(vfioDevices) == 0 {
			return nil, fmt.Errorf("error enumerating all possible devices: %w", npuErr)
		}
		logger.Info("rbln-smi enumeration unavailable, continuing with vfio-pci devices only", "err", npuErr)
		npuDevices = nil
	}
	if len(npuDevices) == 0 && len(vfioDevices) == 0 {
		// The DaemonSet only lands on nodes labelled as having NPUs, so zero
		// devices of either kind means the label, the kernel driver and
		// reality disagree.
		logger.Warn("No NPU devices found on this node",
			"impact", "the published ResourceSlice will be empty and no pod can be scheduled here")
	}
	// Per-device detail is what answers "the slice shows N devices but the
	// node has M". Logged once here rather than in the enumeration, which
	// the rescan loop repeats every tick.
	for _, d := range vfioDevices {
		logger.Debug("Enumerated vfio-pci NPU device",
			"device", d.Name,
			"productName", deviceStringAttr(d, "productName"),
			"pciDeviceID", deviceStringAttr(d, "pciDeviceID"),
			"pciBusID", deviceStringAttr(d, pciBusIDAttributeKey),
			"pcieRoot", deviceStringAttr(d, pcieRootAttributeKey),
			"iommuGroup", deviceIntAttr(d, iommuGroupAttributeKey),
			"numaNode", deviceIntAttr(d, numaNodeAttributeKey),
			"vf", deviceBoolAttr(d, "vf"))
	}

	cdi, err := NewCDIHandler(config.flags.cdiRoot, config.flags.driverName, "npu")
	if err != nil {
		return nil, fmt.Errorf("unable to create CDI handler: %w", err)
	}

	err = cdi.CreateCommonSpecFile(ctx)
	if err != nil {
		if len(npuDevices) > 0 {
			return nil, fmt.Errorf("unable to create CDI spec file for common edits: %w", err)
		}
		// The runtime spec is written by the RBLN container toolkit, which a
		// passthrough-only node does not run. vfio claims never reference
		// the common CDI device, so nothing is degraded.
		logger.Info("Skipping common CDI spec file, RBLN runtime spec unavailable", "err", err)
	}

	checkpointManager, err := checkpointmanager.NewCheckpointManager(config.DriverPluginPath())
	if err != nil {
		return nil, fmt.Errorf("unable to create checkpoint manager: %w", err)
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

// RescanVfioDevices refreshes only the vfio device list; npuDevices stay as
// enumerated at startup. Converting a node to/from vfio-pci therefore relies
// on the plugin being restarted (the current operational procedure) — without
// a restart, a rebound device would be advertised as both npu and vfio.
func (s *DeviceState) RescanVfioDevices(ctx context.Context) (resourceslice.DriverResources, bool, error) {
	vfioDevices, err := enumerateVfioDevices(ctx, s.sysfsPCIDevicesRoot)
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

	// Name the delta: a device that disappears from the slice while a VM is
	// scheduled on it is the thing an operator has to explain, and the ticker
	// gives no other hint of when it happened.
	added, removed := deviceNameDelta(s.vfioDevices, vfioDevices)
	logging.FromContext(ctx).Info("vfio-pci NPU devices changed, republishing resources",
		"added", added, "removed", removed, "vfioDeviceCount", len(vfioDevices))

	s.vfioDevices = vfioDevices
	s.rebuildResources()
	return s.driverResources, true, nil
}

// deviceNameDelta returns the names present only in next (added) and only in
// prev (removed). A device whose attributes changed appears in neither: it is
// still republished, but the record only has to name arrivals and departures.
func deviceNameDelta(prev, next []resourceapi.Device) (added, removed []string) {
	names := func(devices []resourceapi.Device) map[string]struct{} {
		set := make(map[string]struct{}, len(devices))
		for _, d := range devices {
			set[d.Name] = struct{}{}
		}
		return set
	}
	prevNames, nextNames := names(prev), names(next)
	added, removed = []string{}, []string{}
	for _, d := range next {
		if _, ok := prevNames[d.Name]; !ok {
			added = append(added, d.Name)
		}
	}
	for _, d := range prev {
		if _, ok := nextNames[d.Name]; !ok {
			removed = append(removed, d.Name)
		}
	}
	return added, removed
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

	// Write the metadata before the CDI spec: the spec bind-mounts the
	// metadata files, so it must not reference paths that failed to
	// materialize. Any later failure rolls the metadata back by claim UID
	// (owner-marker scan) — metadata left behind by a failed Prepare would
	// otherwise never be collected, because Unprepare no-ops for claims the
	// checkpoint does not record.
	if s.claimHasVfioDevice(claim) {
		if err := writeKubeVirtMetadata(kubevirtMetadataBasePath, claim, s.driverName, s.allocatable); err != nil {
			s.rollbackKubeVirtMetadata(ctx, claimUID)
			return nil, fmt.Errorf("unable to write KubeVirt device metadata: %w", err)
		}
	}

	if err = s.cdi.CreateClaimSpecFile(ctx, claimUID, preparedDevices); err != nil {
		s.rollbackKubeVirtMetadata(ctx, claimUID)
		return nil, fmt.Errorf("unable to create CDI spec file for claim: %w", err)
	}

	preparedClaims[claimUID] = preparedDevices
	if err := s.checkpointManager.CreateCheckpoint(DriverPluginCheckpointFile, checkpoint); err != nil {
		s.rollbackKubeVirtMetadata(ctx, claimUID)
		return nil, fmt.Errorf("unable to sync to checkpoint: %w", err)
	}

	return preparedClaims[claimUID].GetDevices(), nil
}

// rollbackKubeVirtMetadata removes whatever metadata a failed Prepare left
// behind. The prepare error itself is what the caller returns and logs; the
// rollback failure is only logged here because nothing else would mention it,
// and the leftover directory blocks a later claim with the same name.
func (s *DeviceState) rollbackKubeVirtMetadata(ctx context.Context, claimUID string) {
	if err := removeKubeVirtMetadataForClaim(kubevirtMetadataBasePath, claimUID); err != nil {
		logging.FromContext(ctx).Warn("Failed to roll back KubeVirt device metadata after prepare failure",
			"err", err,
			"impact", "a later claim with the same name on this node fails to prepare until the directory is removed")
	}
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

	if err := removeKubeVirtMetadataForClaim(kubevirtMetadataBasePath, claimUID); err != nil {
		return fmt.Errorf("unable to remove KubeVirt device metadata: %w", err)
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
	var rdsNodes []*cdispec.DeviceNode
	if len(npuBusIDs) > 0 {
		hostRsdPath = s.rsdGroupFn(npuBusIDs)
		if hostRsdPath == "" {
			// applyConfig skips the shared node when the path is empty, so the
			// container starts with only its own NPUs, the pod goes Ready, and
			// peer communication is broken with nothing else to show for it.
			// This record is the only signal that the pod is quietly degraded;
			// "impact" states that in the record because the message alone
			// reads like a transient failure the caller retried.
			logger.Error("RSD group creation returned no device path",
				"busIDs", npuBusIDs,
				"impact", "containers get no /dev/rsd0; multi-NPU peer communication disabled")
		} else {
			logger.Info("Created RSD group device", "hostRsdPath", hostRsdPath, "busIDs", npuBusIDs)
		}

		// RDS, like the RSD group, only exists for devices driven by the
		// rebellions kernel driver; a passthrough-only claim never needs it.
		var err error
		rdsNodes, err = s.cdi.getRDSDeviceNodes()
		if err != nil {
			logger.Warn("Reading RDS CDI spec failed, continuing without RDS", "err", err)
		}
	}

	// Metadata is per request: mount its files once, on the request's first
	// vfio device, instead of duplicating them on every device.
	metadataMounted := map[string]bool{}

	var preparedDevices PreparedDevices
	rdsInjected := false
	for _, result := range results {
		var edits *cdispec.ContainerEdits
		var err error

		vfio := isVfioDevice(s.allocatable[result.Device])
		if vfio {
			var metadataFiles []string
			if requestName := kubevirtMetadataRequestName(result); !metadataMounted[requestName] {
				metadataFiles = kubevirtMetadataFiles(kubevirtMetadataBasePath, claim, s.driverName, requestName)
				metadataMounted[requestName] = true
			}
			edits, err = vfioContainerEdits(ctx, s.allocatable[result.Device], metadataFiles)
		} else {
			edits, err = s.applyConfig(ctx, result.Device, hostRsdPath)
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

func devicePCIBusID(device resourceapi.Device) (string, error) {
	attr, ok := device.Attributes[pciBusIDAttributeKey]
	if !ok || attr.StringValue == nil || *attr.StringValue == "" {
		return "", fmt.Errorf("allocatable device %q is missing attribute %s", device.Name, pciBusIDAttributeKey)
	}
	return *attr.StringValue, nil
}

// deviceStringAttr, deviceIntAttr and deviceBoolAttr unwrap an attribute for
// logging. The typed values are pointers, which the text handler would render
// as addresses; nil stands for "unset" so the key still appears in the record.
func deviceStringAttr(device resourceapi.Device, key resourceapi.QualifiedName) string {
	if attr, ok := device.Attributes[key]; ok && attr.StringValue != nil {
		return *attr.StringValue
	}
	return ""
}

func deviceIntAttr(device resourceapi.Device, key resourceapi.QualifiedName) any {
	if attr, ok := device.Attributes[key]; ok && attr.IntValue != nil {
		return *attr.IntValue
	}
	return nil
}

func deviceBoolAttr(device resourceapi.Device, key resourceapi.QualifiedName) any {
	if attr, ok := device.Attributes[key]; ok && attr.BoolValue != nil {
		return *attr.BoolValue
	}
	return nil
}

// setNumaNodeAttr adds the numaNode attribute for a device, or reports why it
// could not. Dropping it silently degrades topology-aware allocation with no
// way to tell from the outside that the device lost its NUMA affinity. The
// value is mirrored under the DraNet key so one claim can matchAttribute NPUs
// with NICs/CPUs published by other drivers.
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
	if v < 0 {
		// sysfs reports -1 for a device without NUMA affinity (single-socket
		// hosts, or firmware that does not expose it). The standard attribute
		// is omitted rather than published as -1, which no CPU or NIC device
		// would match. Expected on such hosts, so debug rather than warn.
		logging.FromContext(ctx).Debug("Device reports no NUMA affinity, omitting numaNode attribute",
			"device", deviceName, "numaNode", numaNode)
		return
	}
	attrs[numaNodeAttributeKey] = resourceapi.DeviceAttribute{IntValue: ptr.To(v)}
	attrs[dranetNumaNodeAttributeKey] = resourceapi.DeviceAttribute{IntValue: ptr.To(v)}
}

// setPCIERootAttr adds the standard pcieRoot attribute ("pci<domain>:<bus>",
// KEP-4381) resolved from sysfs, or reports why it could not. It is derived
// in-driver for npu and vfio devices alike so both enumerations agree and
// cross-driver matchAttribute with DraNet NICs works. Returns the value set,
// or "" when none was.
func setPCIERootAttr(ctx context.Context, attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute, deviceName, sysfsRoot, busID string) string {
	pcieRoot, err := resolvePCIERootID(sysfsRoot, busID)
	if err == nil && pcieRoot == "" {
		err = errors.New("no pci<domain>:<bus> segment in the device path")
	}
	if err != nil {
		logging.FromContext(ctx).Warn("Ignoring unresolvable PCIe root",
			"device", deviceName, "pciBusID", busID, "err", err,
			"impact", "device published without a pcieRoot attribute")
		return ""
	}
	attrs[pcieRootAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(pcieRoot)}
	return pcieRoot
}

func enumerateNpuDevices(ctx context.Context, sysfsRoot string) ([]resourceapi.Device, error) {
	logger := logging.FromContext(ctx)

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
		pcieRoot := setPCIERootAttr(ctx, attrs, d.Name, sysfsRoot, d.PCIBusID)
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
			"pciBusID", d.PCIBusID, "pcieRoot", pcieRoot, "numaNode", d.PCINumaNode,
			"firmwareVersion", d.FirmwareVersion, "driverVersion", d.KMDVersion,
			"memoryTotalBytes", d.MemoryTotalBytes)

		devices = append(devices, device)
	}

	return devices, nil
}
