package main

import (
	"context"
	"path/filepath"

	host "github.com/rbln-sw/rblnlib-go/pkg/host/v1"
	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
	resourceapi "k8s.io/api/resource/v1"
)

const deviceStatusUnreadable = "UNREADABLE"

type deviceHealth struct {
	unhealthy bool
	status    string
}

func healthForDevice(ctx context.Context, root, deviceName string) deviceHealth {
	if root == "" {
		root = "/sys/class/rebellions"
	}
	machine, err := host.New(host.Options{RebellionsClassRoot: root, AccelClassRoot: filepath.Join(filepath.Dir(root), "accel")})
	if err != nil {
		return deviceHealth{status: deviceStatusUnreadable}
	}
	observed, err := machine.ReadNamedState(ctx, deviceName)
	if err != nil {
		logging.FromContext(ctx).Debug("Failed to read device status", "device", deviceName, "err", err)
		return deviceHealth{status: deviceStatusUnreadable}
	}
	return deviceHealth{unhealthy: observed.State != system.StateReady, status: observed.String()}

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
