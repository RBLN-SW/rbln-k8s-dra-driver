package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
	resourceapi "k8s.io/api/resource/v1"
)

// Only READY is available for a new allocation. An unreadable status preserves
// the last allocation health; without a known fault it fails open, including
// on VFs whose kernel driver does not expose this file.
// Values mirror the kernel rbln_dev_status and rblnmlDeviceStatus_t enums.
const (
	deviceStatusReady   = 0
	deviceStatusBusy    = 1
	deviceStatusProbing = 2
	deviceStatusInit    = 3
	deviceStatusFault   = 4
	deviceStatusFinish  = 5
)

const deviceStatusUnreadable = "UNREADABLE"

type deviceHealth struct {
	unhealthy bool
	status    string
}

// deviceStatusName names raw kernel lifecycle states for diagnostics.
func deviceStatusName(status int) string {
	switch status {
	case deviceStatusReady:
		return "READY"
	case deviceStatusBusy:
		return "BUSY"
	case deviceStatusProbing:
		return "PROBING"
	case deviceStatusInit:
		return "INIT"
	case deviceStatusFault:
		return "FAULT"
	case deviceStatusFinish:
		return "FINISH"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", status)
	}
}

func healthForDevice(ctx context.Context, root, deviceName string) deviceHealth {
	if root == "" {
		root = "/sys/class/rebellions"
	}
	statusPath := filepath.Join(root, deviceName, "status")
	raw, err := os.ReadFile(statusPath)
	var status int
	if err == nil {
		status, err = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	if err != nil {
		logging.FromContext(ctx).Debug("Failed to read device status",
			"device", deviceName, "path", statusPath, "err", err)
		return deviceHealth{status: deviceStatusUnreadable}
	}
	return deviceHealth{unhealthy: status != deviceStatusReady, status: deviceStatusName(status)}
}

// Callers hold the state lock (or are initializing it). Scan and Prepare share
// this history so a read failure cannot clear a fault observed by either path.
func (s *DeviceState) checkDeviceHealth(ctx context.Context, d resourceapi.Device) deviceHealth {
	previous, known := s.deviceHealth[d.Name]
	health := healthForDevice(ctx, s.sysfsRebellionsClassRoot, deviceStringAttr(d, deviceNodeNameAttributeKey))
	if health.status == deviceStatusUnreadable {
		health.unhealthy = previous.unhealthy
	}
	if s.deviceHealth == nil {
		s.deviceHealth = make(map[string]deviceHealth)
	}
	s.deviceHealth[d.Name] = health

	logger := logging.FromContext(ctx)
	attrs := []any{"device", d.Name, "pciBusID", deviceStringAttr(d, pciBusIDAttributeKey),
		"sid", deviceStringAttr(d, "sid"), "status", health.status, "previousStatus", previous.status}
	switch {
	case health.unhealthy && (!known || health != previous):
		logger.Warn("NPU is unhealthy, excluding from new allocations", attrs...)
	case !health.unhealthy && previous.unhealthy:
		logger.Info("NPU is available for new allocations again", attrs...)
	}
	return health
}

// Callers hold the state lock (or are initializing it). Keep unhealthy devices
// in discovery so they can recover and remain known during inventory errors.
// VFIO devices have no native status and are not checked here.
func (s *DeviceState) refreshDeviceHealth(ctx context.Context) {
	next := make(map[string]deviceHealth, len(s.npuDevices))
	for _, d := range s.npuDevices {
		next[d.Name] = s.checkDeviceHealth(ctx, d)
	}
	s.deviceHealth = next
}
