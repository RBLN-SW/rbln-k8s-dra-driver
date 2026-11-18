package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"

	resourceapi "k8s.io/api/resource/v1"
)

// Discovery can lag a removal, rebind or repartition. Validate the allocated
// device before creating CDI/RSD state to cover changes between scan ticks or
// while scanning is disabled. DeviceClass policy stays in selectors.
func validateAllocatedDevice(ctx context.Context, root string, d resourceapi.Device) error {
	bus, err := devicePCIBusID(d)
	if err != nil {
		return err
	}
	devPath := filepath.Join(root, strings.ToLower(bus))
	machine, err := systemHost(root)
	if err != nil {
		return err
	}
	observed, err := machine.GetDevice(ctx, bus)
	if err != nil {
		return err
	}
	wantDriver := nativePCIDriverName
	if isVfioDevice(d) {
		wantDriver = vfioPCIDriverName
	}
	if !observed.DriverKnown {
		return system.IssuesError(observed.Issues)
	}
	driver := observed.Driver
	if driver != wantDriver {
		return fmt.Errorf("PCI driver for %s is %q, expected %q", bus, driver, wantDriver)
	}
	info, err := probeSRIOV(ctx, root, bus)
	if err != nil {
		return err
	}
	if info.isHostingPF() || info.partitionMode() != deviceStringAttr(d, partitionModeAttributeKey) ||
		!strings.EqualFold(info.parentPCIBusID, deviceStringAttr(d, parentPCIBusIDAttributeKey)) {
		return fmt.Errorf("SR-IOV layout changed for %s", bus)
	}
	if isVfioDevice(d) {
		group, err := isolatedIOMMUGroup(ctx, devPath, bus)
		if err != nil {
			return err
		}
		if deviceIntAttr(d, iommuGroupAttributeKey) != group {
			return fmt.Errorf("IOMMU group changed for %s", bus)
		}
	}
	return nil
}
