package main

import (
	"fmt"
	"path/filepath"
	"strings"

	resourceapi "k8s.io/api/resource/v1"
)

// Discovery can lag a removal, rebind or repartition. Validate the allocated
// device before creating CDI/RSD state to cover changes between scan ticks or
// while scanning is disabled. DeviceClass policy stays in selectors.
func validateAllocatedDevice(root string, d resourceapi.Device) error {
	bus, err := devicePCIBusID(d)
	if err != nil {
		return err
	}
	devPath := filepath.Join(root, strings.ToLower(bus))
	vendor, err := readSysfsValue(devPath, "vendor")
	if err != nil {
		return err
	}
	if !strings.EqualFold(vendor, rblnPCIVendorID) {
		return fmt.Errorf("PCI vendor changed for %s", bus)
	}
	wantDriver := nativePCIDriverName
	if isVfioDevice(d) {
		wantDriver = vfioPCIDriverName
	}
	driver, err := readPCIDriver(devPath)
	if err != nil {
		return err
	}
	if driver != wantDriver {
		return fmt.Errorf("PCI driver for %s is %q, expected %q", bus, driver, wantDriver)
	}
	info, err := probeSRIOV(root, bus)
	if err != nil {
		return err
	}
	if info.isHostingPF() || info.partitionMode() != deviceStringAttr(d, partitionModeAttributeKey) ||
		!strings.EqualFold(info.parentPCIBusID, deviceStringAttr(d, parentPCIBusIDAttributeKey)) {
		return fmt.Errorf("SR-IOV layout changed for %s", bus)
	}
	if isVfioDevice(d) {
		group, err := isolatedIOMMUGroup(devPath, bus)
		if err != nil {
			return err
		}
		if deviceIntAttr(d, iommuGroupAttributeKey) != group {
			return fmt.Errorf("IOMMU group changed for %s", bus)
		}
	}
	return nil
}
