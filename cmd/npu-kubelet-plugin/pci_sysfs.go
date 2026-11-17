/*
 * Copyright 2025 The Kubernetes Authors.
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
	"path/filepath"

	host "github.com/rbln-sw/rblnlib-go/pkg/host/v1"
	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"
)

const (
	sysfsPCIDevicesRoot = "/sys/bus/pci/devices"
	nativePCIDriverName = system.NativeDriver
)

func systemHost(root string) (*host.Client, error) {
	if root == "" {
		root = sysfsPCIDevicesRoot
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return host.New(host.Options{PCIDevicesRoot: absolute})
}
func systemInventory(ctx context.Context, root string, known map[string]bool) (system.Snapshot, error) {
	c, err := systemHost(root)
	if err != nil {
		return system.Snapshot{}, err
	}
	s, err := c.ListDevices(ctx)
	if err != nil {
		return s, err
	}
	for _, issue := range s.Issues {
		var e *system.Error
		if errors.As(issue.Err, &e) && e.Op == "device" && e.Kind == system.NotFound {
			continue
		}
		// Unknown functions with unreadable identity have never been advertised.
		if !known[issue.Target] {
			continue
		}
		return s, issue.Err
	}
	return s, nil
}

func resolvePCIERootID(root, busID string) (string, error) {
	c, err := systemHost(root)
	if err != nil {
		return "", err
	}
	paths, err := c.PCIAncestors(context.Background(), busID)
	if err != nil {
		return "", err
	}
	return filepath.Base(paths[0]), nil
}
