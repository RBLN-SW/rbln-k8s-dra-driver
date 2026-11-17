package main

import (
	"context"
	"errors"
	"fmt"

	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/utils/ptr"
)

const (
	parentPCIBusIDAttributeKey = resourceapi.QualifiedName("parentPCIBusID")
	partitionModeAttributeKey  = resourceapi.QualifiedName("partitionMode")
	numVFsAttributeKey         = resourceapi.QualifiedName("numVFs")
	deviceNodeNameAttributeKey = resourceapi.QualifiedName("deviceNodeName")
)

// A successfully observed but unsupported layout can be omitted. Read or
// parse failures must instead abort discovery, preserving the last snapshot.
var errUnsupportedDevice = errors.New("unsupported device configuration")

type sriovInfo struct {
	parentPCIBusID string
	numVFs         int64
}

func (s sriovInfo) isVF() bool        { return s.parentPCIBusID != "" }
func (s sriovInfo) isHostingPF() bool { return !s.isVF() && s.numVFs > 0 }

// A physfn symlink identifies a VF; its parent's sriov_numvfs defines the
// partition mode. A PF with enabled VFs is not a compute endpoint. Missing PCI
// entries and incomplete or unsupported VF configurations fail closed.
func probeSRIOV(root, busID string) (sriovInfo, error) {
	c, err := systemHost(root)
	if err != nil {
		return sriovInfo{}, err
	}
	fact, err := c.ReadSRIOV(context.Background(), busID)
	if err != nil {
		return sriovInfo{}, err
	}
	count := int64(fact.EnabledVFs)
	if fact.ParentEnabledVFs != nil {
		count = int64(*fact.ParentEnabledVFs)
	}
	if fact.Kind == system.VirtualFunction && count != 1 && count != 4 {
		return sriovInfo{}, fmt.Errorf("%w: PF %s has %d VFs", errUnsupportedDevice, fact.ParentPCIAddress, count)
	}
	if fact.Kind == system.PhysicalFunction && count != 0 && count != 1 && count != 4 {
		return sriovInfo{}, fmt.Errorf("%w: PF %s has %d VFs", errUnsupportedDevice, busID, count)
	}
	return sriovInfo{parentPCIBusID: fact.ParentPCIAddress, numVFs: count}, nil
}

func (s sriovInfo) partitionMode() string {
	if s.isVF() {
		return fmt.Sprintf("vf%d", s.numVFs)
	}
	return "none"
}

// PF names stay compatible; a different VF profile is a different logical
// device even when the kernel reuses the same BDF or /dev name.
func (s sriovInfo) deviceName(base string) string {
	if s.isVF() {
		return base + "-" + s.partitionMode()
	}
	return base
}

func (s sriovInfo) setAttributes(attrs map[resourceapi.QualifiedName]resourceapi.DeviceAttribute) {
	attrs[vfAttributeKey] = resourceapi.DeviceAttribute{BoolValue: ptr.To(s.isVF())}
	if s.isVF() {
		attrs[parentPCIBusIDAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.parentPCIBusID)}
	}
	attrs[partitionModeAttributeKey] = resourceapi.DeviceAttribute{StringValue: ptr.To(s.partitionMode())}
	attrs[numVFsAttributeKey] = resourceapi.DeviceAttribute{IntValue: ptr.To(s.numVFs)}
}
