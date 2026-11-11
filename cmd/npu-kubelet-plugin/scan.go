package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/dynamic-resource-allocation/resourceslice"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
	"github.com/rbln-sw/rblnlib-go/pkg/device"
)

// scanStatus remembers which failure records have already been emitted so
// the loop logs transitions, not every tick.
type scanStatus struct {
	sysfsFailing bool
	npuFailing   bool
	slow         bool
}

// observeScanDuration is the only default-level signal that the interval is
// too aggressive for a node. The loop cannot pile up (the ticker coalesces
// missed ticks), so this is a warning about cost, not correctness.
func (s *DeviceState) observeScanDuration(ctx context.Context, took, interval time.Duration) {
	logger := logging.FromContext(ctx)
	slow := took > interval
	switch {
	case slow && !s.scan.slow:
		logger.Warn("Device scan slower than its interval",
			"durationMs", took.Milliseconds(), "interval", interval.String(),
			"impact", "rbln-smi runs back to back on this node; raise DEVICE_SCAN_INTERVAL if that is a problem")
	case !slow && s.scan.slow:
		logger.Info("Device scan duration back under its interval",
			"durationMs", took.Milliseconds(), "interval", interval.String())
	}
	s.scan.slow = slow
}

// RescanDevices re-reads both device kinds from the same tick's sysfs state
// and republishes only when the published inventory changes. rbln-smi runs only
// when sysfs shows a native candidate (see countNativeCandidates), so a
// VM-passthrough node never executes it. Enumeration runs outside the lock;
// the swap under it is whole-list, so Prepare sees either the previous or
// the next inventory, never a mix.
//
// A failed inventory read keeps the last successful discovery snapshot.
// Never combine old native devices with newly scanned VFIO devices:
// a rebound PCI function could otherwise be published under both names.
// Health is still refreshed for that snapshot, so an inventory failure does
// not hide a device fault. A successful empty scan removes devices normally.
func (s *DeviceState) RescanDevices(ctx context.Context) (resourceslice.DriverResources, bool, error) {
	logger := logging.FromContext(ctx)
	start := time.Now()

	// Reuse discovery, including unhealthy devices, to distinguish an unknown
	// vendor from a temporarily unreadable NPU; no separate cache is needed.
	known := map[string]bool{}
	s.Lock()
	for _, d := range slices.Concat(s.npuDevices, s.vfioDevices) {
		for _, key := range []resourceapi.QualifiedName{pciBusIDAttributeKey, parentPCIBusIDAttributeKey} {
			if bus := deviceStringAttr(d, key); bus != "" {
				known[strings.ToLower(bus)] = true
			}
		}
	}
	s.Unlock()
	vfioDevices, vfioSkipped, err := enumerateVfioDevices(ctx, s.sysfsPCIDevicesRoot, known)
	var candidates int
	if err == nil {
		candidates, err = countNativeCandidates(s.sysfsPCIDevicesRoot, known)
	}
	if err != nil {
		if !s.scan.sysfsFailing {
			logger.Error("Device scan failed, keeping last inventory", "err", err,
				"impact", "device identities stay unchanged until a scan succeeds; native health is still checked")
		}
		s.scan.sysfsFailing = true
		return s.resourcesAfterScanError(ctx, err)
	} else if s.scan.sysfsFailing {
		logger.Info("Device scan recovered")
		s.scan.sysfsFailing = false
	}

	var npuDevices []resourceapi.Device
	var skipped []skippedDevice
	var inventoryErr error
	if candidates > 0 {
		var devs []device.Device
		devs, inventoryErr = s.inventoryFn(ctx)
		if inventoryErr == nil {
			npuDevices, skipped, inventoryErr = npuDevicesFromInventory(ctx, s.sysfsPCIDevicesRoot, devs)
		}
		switch {
		case inventoryErr != nil && !s.scan.npuFailing:
			logger.Error("NPU enumeration failed, keeping last inventory", "err", inventoryErr,
				"impact", "neither native nor passthrough inventory changes until a complete scan succeeds")
		case inventoryErr == nil && s.scan.npuFailing:
			logger.Info("NPU enumeration recovered")
		}
		s.scan.npuFailing = inventoryErr != nil
		if inventoryErr != nil {
			return s.resourcesAfterScanError(ctx, inventoryErr)
		}
	} else {
		// No native query is needed this tick. A later failure starts a new streak.
		s.scan.npuFailing = false
	}
	skipped = append(skipped, vfioSkipped...)
	// Binding can change between the sysfs and SMI reads. Reject an
	// inconsistent snapshot instead of offering one function twice.
	seen := map[string]bool{}
	for _, d := range slices.Concat(npuDevices, vfioDevices) {
		bus := strings.ToLower(deviceStringAttr(d, pciBusIDAttributeKey))
		if seen[bus] {
			err := fmt.Errorf("PCI function %s appeared in both discovery results", bus)
			logger.Error("Discarding inconsistent device scan", "err", err)
			return s.resourcesAfterScanError(ctx, err)
		}
		seen[bus] = true
	}

	s.Lock()
	defer s.Unlock()
	previousResources := s.driverResources
	changed := !equality.Semantic.DeepEqual(npuDevices, s.npuDevices) || !equality.Semantic.DeepEqual(vfioDevices, s.vfioDevices)
	if changed {
		added, removed, renamed := deviceNameDelta(slices.Concat(s.npuDevices, s.vfioDevices), slices.Concat(npuDevices, vfioDevices))
		logger.Info("Device inventory changed, republishing resources",
			"added", added, "removed", removed, "changed", renamed,
			"npuDeviceCount", len(npuDevices), "vfioDeviceCount", len(vfioDevices))
		// A skip is news on the tick the device leaves the inventory; a device
		// that stays skipped was reported then or at startup.
		for _, d := range skipped {
			for _, previous := range slices.Concat(s.npuDevices, s.vfioDevices) {
				if slices.Contains(removed, previous.Name) && strings.EqualFold(deviceStringAttr(previous, pciBusIDAttributeKey), d.pciBusID) {
					logSkippedDevices(ctx, []skippedDevice{d})
					break
				}
			}
		}
		s.npuDevices, s.vfioDevices = npuDevices, vfioDevices
	}
	s.refreshDeviceHealth(ctx)
	s.rebuildResources()
	changed = !equality.Semantic.DeepEqual(previousResources, s.driverResources)

	// The only per-tick record: it tells a quiet loop from a dead one.
	reasons := make(map[string]string, len(skipped))
	for _, d := range skipped {
		reasons[d.name] = d.reason()
	}
	attrs := []any{
		"npuDeviceCount", len(s.npuDevices), "vfioDeviceCount", len(s.vfioDevices),
		"nativeCandidates", candidates, "rblnSmiRun", candidates > 0,
		"skipped", reasons, "changed", changed, "durationMs", time.Since(start).Milliseconds(),
	}
	logger.Debug("Scanned device inventory", attrs...)
	return s.driverResources, changed, nil
}

// Inventory failures preserve identities, but must not leave a known fault
// allocatable merely because rbln-smi or PCI discovery is also failing.
func (s *DeviceState) resourcesAfterScanError(ctx context.Context, scanErr error) (resourceslice.DriverResources, bool, error) {
	s.Lock()
	defer s.Unlock()
	previousResources := s.driverResources
	s.refreshDeviceHealth(ctx)
	s.rebuildResources()
	return s.driverResources, !equality.Semantic.DeepEqual(previousResources, s.driverResources), scanErr
}

// skippedDevice is a native or VFIO inventory entry that enumeration did not
// publish. err is nil for the one expected case, a VF-hosting PF.
type skippedDevice struct {
	name, pciBusID string
	sriov          sriovInfo
	err            error
}

func (d skippedDevice) reason() string {
	if d.err != nil {
		return d.err.Error()
	}
	return fmt.Sprintf("VF-hosting PF with %d VFs", d.sriov.numVFs)
}

// logSkippedDevices reports each skipped device once. Enumeration itself is
// silent because it runs every scan tick; callers log at startup and when a
// device newly drops out of the inventory.
func logSkippedDevices(ctx context.Context, skipped []skippedDevice) {
	logger := logging.FromContext(ctx)
	for _, d := range skipped {
		if d.err != nil {
			logger.Error("Skipping NPU not usable in its current PCI state",
				"device", d.name, "pciBusID", d.pciBusID, "err", d.err,
				"impact", "device is not published until a later scan sees a usable PCI state")
			continue
		}
		logger.Info("Excluding VF-hosting PF from advertisement",
			"device", d.name, "pciBusID", d.pciBusID, "numVFs", d.sriov.numVFs)
	}
}

// countNativeCandidates reports how many Rebellions PCI functions the kernel
// driver could be exposing: bound to rebellions and not
// hosting SR-IOV VFs. Zero means rbln-smi has nothing to report, which is the
// state of a VM-passthrough node in both PF and VF mode. A VF-hosting PF
// remains a control function even when bound to the native driver. Required
// reads of known Rebellions functions fail the scan instead of reporting zero.
func countNativeCandidates(root string, known map[string]bool) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", root, err)
	}
	count := 0
	for _, entry := range entries {
		if !pciAddressRegexp.MatchString(entry.Name()) {
			continue
		}
		devPath := filepath.Join(root, entry.Name())
		matches, err := isRebellionsPCI(devPath, known)
		if err != nil {
			return 0, err
		}
		if !matches {
			continue
		}
		driver, err := readPCIDriver(devPath)
		if err != nil {
			return 0, err
		}
		if driver != nativePCIDriverName {
			continue
		}
		numVFs, err := readSRIOVNumVFs(devPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		if numVFs > 0 {
			continue
		}
		count++
	}
	return count, nil
}
