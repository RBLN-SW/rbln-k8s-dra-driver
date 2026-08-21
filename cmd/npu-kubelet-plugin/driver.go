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

	// Logged before the call so a publish failure is not silent.
	logging.FromContext(ctx).Info("Publishing node devices",
		"deviceCount", len(state.allocatable), "pool", config.flags.nodeName)
	if err := helper.PublishResources(ctx, state.driverResources); err != nil {
		return nil, err
	}

	return driver, nil
}

func (d *driver) Shutdown() error {
	if d.healthcheck != nil {
		d.healthcheck.Stop()
	}
	d.helper.Stop()
	return nil
}

func (d *driver) PrepareResourceClaims(ctx context.Context, claims []*resourceapi.ResourceClaim) (map[types.UID]kubeletplugin.PrepareResult, error) {
	logging.FromContext(ctx).Info("Received request to prepare resource claims", "count", len(claims))
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
	logging.FromContext(ctx).Info("Received request to unprepare resource claims", "count", len(claims))
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
