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
	"time"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	coreclientset "k8s.io/client-go/kubernetes"
	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

type driver struct {
	client      coreclientset.Interface
	helper      *kubeletplugin.Helper
	state       *DeviceState
	healthcheck *healthcheck
	cancelCtx   func(error)
}

func NewDriver(ctx context.Context, config *Config) (*driver, error) {
	driver := &driver{
		client:    config.coreclient,
		cancelCtx: config.cancelMainCtx,
	}

	state, err := NewDeviceState(ctx, config)
	if err != nil {
		return nil, err
	}
	driver.state = state

	helper, err := kubeletplugin.Start(
		ctx,
		driver,
		kubeletplugin.KubeClient(config.coreclient),
		kubeletplugin.NodeName(config.flags.nodeName),
		kubeletplugin.DriverName(config.flags.driverName),
		kubeletplugin.RegistrarDirectoryPath(config.flags.kubeletRegistrarDirectoryPath),
		kubeletplugin.PluginDataDirectoryPath(config.DriverPluginPath()),
	)
	if err != nil {
		return nil, err
	}
	driver.helper = helper

	driver.healthcheck, err = startHealthcheck(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("start healthcheck: %w", err)
	}

	if err := helper.PublishResources(ctx, state.DriverResources()); err != nil {
		return nil, err
	}

	// The last startup record, and the one that says the plugin is serving:
	// enumeration, kubelet registration, the healthcheck and the publish
	// request all succeeded. PublishResources does not block and the apiserver
	// rejects invalid slices asynchronously, so this claims the driver is up,
	// not that the ResourceSlice exists yet — a rejected write arrives later
	// through HandleError.
	logging.FromContext(ctx).Info("Driver started",
		"driverName", config.flags.driverName, "pool", config.flags.nodeName,
		"deviceCount", len(state.allocatable),
		"npuDeviceCount", len(state.npuDevices), "vfioDeviceCount", len(state.vfioDevices),
		"vfioRescanInterval", config.flags.vfioRescanInterval.String())

	// Started after the startup record so the counts above are read before the
	// rescan loop can start rewriting them.
	if interval := config.flags.vfioRescanInterval; interval > 0 {
		go driver.runVfioRescan(ctx, interval)
	}

	return driver, nil
}

// runVfioRescan republishes the ResourceSlice whenever the set of vfio-pci
// bound NPUs changes. Only vfio devices are rescanned (see
// DeviceState.RescanVfioDevices); a failed scan is retried on the next tick.
func (d *driver) runVfioRescan(ctx context.Context, interval time.Duration) {
	logger := logging.FromContext(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		resources, changed, err := d.state.RescanVfioDevices(ctx)
		if err != nil {
			logger.Error("Failed to rescan vfio-pci NPU devices", "err", err,
				"impact", "published passthrough devices may be stale until the next rescan succeeds")
			continue
		}
		if !changed {
			continue
		}

		if err := d.helper.PublishResources(ctx, resources); err != nil {
			logger.Error("Failed to publish resources after vfio-pci rescan", "err", err,
				"impact", "the ResourceSlice does not reflect the current passthrough devices")
		}
	}
}

func (d *driver) Shutdown() error {
	if d.healthcheck != nil {
		d.healthcheck.Stop()
	}
	d.helper.Stop()
	return nil
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	// The liveness probe drives this RPC with an empty claim list once per
	// probe period (health.go), and the helper forwards it here unconditionally.
	// Logging those at info would drown real kubelet traffic in records that
	// look exactly like it.
	if len(claims) > 0 {
		logging.FromContext(ctx).Info("Received request to prepare resource claims", "count", len(claims))
	}
	result := make(map[types.UID]kubeletplugin.PrepareResult)

	for _, claim := range claims {
		result[claim.UID] = d.prepareResourceClaim(ctx, claim)
	}

	return result, nil
}

func (d *driver) prepareResourceClaim(ctx context.Context, claim *resourceapi.ResourceClaim) kubeletplugin.PrepareResult {
	// Identify the claim the way an operator holds it — they start from a
	// pending pod, not a UID — and let everything downstream inherit it.
	ctx = logging.WithValues(ctx, "claimUID", string(claim.UID),
		"claimNamespace", claim.Namespace, "claimName", claim.Name)
	logger := logging.FromContext(ctx)

	preparedPBs, err := d.state.Prepare(ctx, claim)
	if err != nil {
		// Logged here as well as returned: kubelet surfaces the error on the
		// pod, but an operator reading only the driver's log would otherwise
		// see nothing at all for a claim that never becomes ready.
		logger.Error("Failed to prepare devices for claim", "err", err)
		return kubeletplugin.PrepareResult{
			Err: fmt.Errorf("error preparing devices for claim %v: %w", claim.UID, err),
		}
	}
	var prepared []kubeletplugin.Device
	for _, preparedPB := range preparedPBs {
		prepared = append(prepared, kubeletplugin.Device{
			Requests:     preparedPB.GetRequestNames(),
			PoolName:     preparedPB.GetPoolName(),
			DeviceName:   preparedPB.GetDeviceName(),
			CDIDeviceIDs: preparedPB.GetCdiDeviceIds(),
		})
	}

	deviceNames := make([]string, 0, len(prepared))
	for _, p := range prepared {
		deviceNames = append(deviceNames, p.DeviceName)
	}
	logger.Info("Prepared devices for claim", "devices", deviceNames)
	logger.Debug("Prepared device details", "devices", prepared)
	return kubeletplugin.PrepareResult{Devices: prepared}
}

func (d *driver) UnprepareResourceClaims(ctx context.Context, claims []kubeletplugin.NamespacedObject) (map[types.UID]error, error) {
	// Guarded for the same reason as prepare, so the two stay symmetric.
	if len(claims) > 0 {
		logging.FromContext(ctx).Info("Received request to unprepare resource claims", "count", len(claims))
	}
	result := make(map[types.UID]error)

	for _, claim := range claims {
		result[claim.UID] = d.unprepareResourceClaim(ctx, claim)
	}

	return result, nil
}

func (d *driver) unprepareResourceClaim(ctx context.Context, claim kubeletplugin.NamespacedObject) error {
	ctx = logging.WithValues(ctx, "claimUID", string(claim.UID),
		"claimNamespace", claim.Namespace, "claimName", claim.Name)

	if err := d.state.Unprepare(ctx, string(claim.UID)); err != nil {
		// Same reasoning as prepare: a leaked device node is invisible to an
		// operator who only has the driver's log.
		logging.FromContext(ctx).Error("Failed to unprepare devices for claim", "err", err)
		return fmt.Errorf("error unpreparing devices for claim %v: %w", claim.UID, err)
	}

	return nil
}

func (d *driver) HandleError(ctx context.Context, err error, msg string) {
	utilruntime.HandleErrorWithContext(ctx, err, msg)
	if !errors.Is(err, kubeletplugin.ErrRecoverable) && d.cancelCtx != nil {
		d.cancelCtx(fmt.Errorf("fatal background error: %w", err))
	}
}
