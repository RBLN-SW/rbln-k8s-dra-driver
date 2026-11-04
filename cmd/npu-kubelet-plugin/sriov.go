package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
	if !pciAddressRegexp.MatchString(busID) {
		return sriovInfo{}, fmt.Errorf("invalid PCI address %q", busID)
	}
	// Normalize only the sysfs lookup; inventory IDs are also used for RSD matching.
	devPath := filepath.Join(root, strings.ToLower(busID))
	if _, err := os.Stat(devPath); err != nil {
		return sriovInfo{}, fmt.Errorf("stat PCI device %s: %w", busID, err)
	}
	if _, statErr := os.Lstat(filepath.Join(devPath, "physfn")); statErr == nil {
		parent, err := readSysfsLinkBase(devPath, "physfn")
		if err != nil {
			return sriovInfo{}, fmt.Errorf("read physfn of %s: %w", busID, err)
		}
		if !pciAddressRegexp.MatchString(parent) || strings.EqualFold(parent, busID) {
			return sriovInfo{}, fmt.Errorf("invalid parent PF %q for VF %s", parent, busID)
		}
		numVFs, err := readSRIOVNumVFs(filepath.Join(root, parent))
		if err != nil {
			return sriovInfo{}, fmt.Errorf("read parent PF %s for VF %s: %w", parent, busID, err)
		}
		if numVFs != 1 && numVFs != 4 {
			return sriovInfo{}, fmt.Errorf("%w: PF %s has %d VFs (expected 1 or 4)", errUnsupportedDevice, parent, numVFs)
		}
		return sriovInfo{parentPCIBusID: parent, numVFs: numVFs}, nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return sriovInfo{}, fmt.Errorf("stat physfn of %s: %w", busID, statErr)
	}
	numVFs, err := readSRIOVNumVFs(devPath)
	if errors.Is(err, os.ErrNotExist) {
		// Older hardware does not expose SR-IOV attributes.
		return sriovInfo{}, nil
	}
	if err != nil {
		return sriovInfo{}, err
	}
	if numVFs != 0 && numVFs != 1 && numVFs != 4 {
		return sriovInfo{}, fmt.Errorf("%w: PF %s has %d VFs (expected 0, 1 or 4)", errUnsupportedDevice, busID, numVFs)
	}
	return sriovInfo{numVFs: numVFs}, nil
}

func readSRIOVNumVFs(devPath string) (int64, error) {
	raw, err := readSysfsValue(devPath, "sriov_numvfs")
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid sriov_numvfs %q in %s", raw, devPath)
	}
	return n, nil
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
