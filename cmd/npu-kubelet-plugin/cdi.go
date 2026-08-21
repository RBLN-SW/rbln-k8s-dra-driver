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
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/logging"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	cdiCommonDeviceName   = "common"
	rblnRuntimeSpecFile   = "rbln.yaml"
	rblnRuntimeSpecKind   = "rebellions.ai/npu"
	rblnRuntimeDeviceName = "runtime"
)

type CDIHandler struct {
	cache      *cdiapi.Cache
	root       string
	driverName string
	class      string
}

func NewCDIHandler(root string, driverName, class string) (*CDIHandler, error) {
	cache, err := cdiapi.NewCache(
		cdiapi.WithSpecDirs(root),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create a new CDI cache: %w", err)
	}
	handler := &CDIHandler{
		cache:      cache,
		root:       root,
		driverName: driverName,
		class:      class,
	}

	return handler, nil
}

func (cdi *CDIHandler) CreateCommonSpecFile(ctx context.Context) error {
	mounts, hooks, err := cdi.getRuntimeUMDEdits()
	if err != nil {
		return fmt.Errorf("failed to get runtime UMD edits: %w", err)
	}

	spec := &cdispec.Spec{
		Kind: cdi.kind(),
		Devices: []cdispec.Device{
			{
				Name: cdiCommonDeviceName,
				ContainerEdits: cdispec.ContainerEdits{
					Mounts: mounts,
					Hooks:  hooks,
				},
			},
		},
	}

	minVersion, err := cdiapi.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %w", err)
	}
	spec.Version = minVersion

	specName, err := cdiapi.GenerateNameForTransientSpec(spec, cdiCommonDeviceName)
	if err != nil {
		return fmt.Errorf("failed to generate Spec name: %w", err)
	}

	if err := cdi.cache.WriteSpec(spec, specName); err != nil {
		return err
	}
	// "The container cannot find the userspace library" starts with whether the
	// toolkit's edits were picked up at all.
	logging.FromContext(ctx).Debug("Wrote common CDI spec with runtime edits",
		"specName", specName, "cdiRoot", cdi.root, "mounts", len(mounts), "hooks", len(hooks))
	return nil
}

type rblnRuntimeSpec struct {
	Kind    string              `yaml:"kind"`
	Devices []rblnRuntimeDevice `yaml:"devices"`
}

type rblnRuntimeDevice struct {
	Name           string                 `yaml:"name"`
	ContainerEdits rblnRuntimeDeviceEdits `yaml:"containerEdits"`
}

type rblnRuntimeDeviceEdits struct {
	Mounts []rblnRuntimeMount `yaml:"mounts"`
	Hooks  []rblnRuntimeHook  `yaml:"hooks"`
}

type rblnRuntimeMount struct {
	HostPath      string   `yaml:"hostPath"`
	ContainerPath string   `yaml:"containerPath"`
	Options       []string `yaml:"options"`
	Type          string   `yaml:"type"`
}

type rblnRuntimeHook struct {
	HookName string   `yaml:"hookname"`
	Path     string   `yaml:"path"`
	Args     []string `yaml:"args"`
	Env      []string `yaml:"env"`
	Timeout  *int     `yaml:"timeout"`
}

func (cdi *CDIHandler) getRuntimeUMDEdits() ([]*cdispec.Mount, []*cdispec.Hook, error) {
	specPath := filepath.Join(cdi.root, rblnRuntimeSpecFile)
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read %s: %w", specPath, err)
	}

	var runtimeSpec rblnRuntimeSpec
	if err := yaml.Unmarshal(specBytes, &runtimeSpec); err != nil {
		return nil, nil, fmt.Errorf("failed to parse %s: %w", specPath, err)
	}
	if runtimeSpec.Kind != rblnRuntimeSpecKind {
		return nil, nil, fmt.Errorf("unexpected CDI kind %q in %s", runtimeSpec.Kind, specPath)
	}

	for _, device := range runtimeSpec.Devices {
		if device.Name != rblnRuntimeDeviceName {
			continue
		}

		mounts := make([]*cdispec.Mount, 0, len(device.ContainerEdits.Mounts))
		for _, mount := range device.ContainerEdits.Mounts {
			mounts = append(mounts, &cdispec.Mount{
				HostPath:      mount.HostPath,
				ContainerPath: mount.ContainerPath,
				Options:       append([]string{}, mount.Options...),
				Type:          mount.Type,
			})
		}

		hooks := make([]*cdispec.Hook, 0, len(device.ContainerEdits.Hooks))
		for _, hook := range device.ContainerEdits.Hooks {
			hooks = append(hooks, &cdispec.Hook{
				HookName: hook.HookName,
				Path:     hook.Path,
				Args:     append([]string{}, hook.Args...),
				Env:      append([]string{}, hook.Env...),
				Timeout:  hook.Timeout,
			})
		}

		return mounts, hooks, nil
	}

	return nil, nil, fmt.Errorf("runtime device %q not found in %s", rblnRuntimeDeviceName, specPath)
}

func (cdi *CDIHandler) CreateClaimSpecFile(ctx context.Context, claimUID string, devices PreparedDevices) error {
	specName := cdiapi.GenerateTransientSpecName(cdi.vendor(), cdi.class, claimUID)

	spec := &cdispec.Spec{
		Kind:    cdi.kind(),
		Devices: []cdispec.Device{},
	}

	for _, device := range devices {
		claimEdits := cdiapi.ContainerEdits{
			ContainerEdits: &cdispec.ContainerEdits{},
		}
		claimEdits.Append(device.ContainerEdits)

		cdiDevice := cdispec.Device{
			Name:           fmt.Sprintf("%s-%s", claimUID, device.DeviceName),
			ContainerEdits: *claimEdits.ContainerEdits,
		}

		spec.Devices = append(spec.Devices, cdiDevice)
	}

	minVersion, err := cdiapi.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %w", err)
	}
	spec.Version = minVersion

	if err := cdi.cache.WriteSpec(spec, specName); err != nil {
		return err
	}
	// CDI is the step between "the driver decided" and "the container can see
	// it", so which host paths went in is what distinguishes a driver bug from
	// a runtime that ignored the spec.
	nodes := make([]string, 0, len(devices))
	for _, device := range devices {
		if device.ContainerEdits == nil || device.ContainerEdits.ContainerEdits == nil {
			continue
		}
		for _, node := range device.ContainerEdits.DeviceNodes {
			nodes = append(nodes, node.Path+"="+node.HostPath)
		}
	}
	logging.FromContext(ctx).Debug("Wrote CDI spec for claim",
		"specName", specName, "cdiRoot", cdi.root, "deviceNodes", nodes)
	return nil
}

func (cdi *CDIHandler) DeleteClaimSpecFile(ctx context.Context, claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdi.vendor(), cdi.class, claimUID)
	if err := cdi.cache.RemoveSpec(specName); err != nil {
		return err
	}
	logging.FromContext(ctx).Debug("Removed CDI spec for claim", "specName", specName)
	return nil
}

func (cdi *CDIHandler) GetClaimDevices(claimUID string, devices []string) []string {
	cdiDevices := []string{
		cdiparser.QualifiedName(cdi.vendor(), cdi.class, cdiCommonDeviceName),
	}

	for _, device := range devices {
		cdiDevice := cdiparser.QualifiedName(cdi.vendor(), cdi.class, fmt.Sprintf("%s-%s", claimUID, device))
		cdiDevices = append(cdiDevices, cdiDevice)
	}

	return cdiDevices
}

func (cdi *CDIHandler) kind() string {
	return cdi.vendor() + "/" + cdi.class
}

func (cdi *CDIHandler) vendor() string {
	return "k8s." + cdi.driverName
}
