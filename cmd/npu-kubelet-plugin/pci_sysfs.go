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
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const sysfsPCIDevicesRoot = "/sys/bus/pci/devices"

var pciAddressRegexp = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// pciRootRegexp matches the root complex segment ("pci<domain>:<bus>") that
// leads a device's /sys/devices path, the value format required by the
// standard resource.kubernetes.io/pcieRoot attribute (KEP-4381).
var pciRootRegexp = regexp.MustCompile(`^pci[0-9a-fA-F]{4}:[0-9a-fA-F]{2}$`)

func readSysfsValue(devPath, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(devPath, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func readSysfsLinkBase(devPath, name string) (string, error) {
	target, err := os.Readlink(filepath.Join(devPath, name))
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// resolvePCIERootID resolves the PCIe root complex ("pci<domain>:<bus>") a
// device hangs off of by walking the physical device path in sysfs.
func resolvePCIERootID(root, busID string) (string, error) {
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(root, busID))
	if err != nil {
		return "", err
	}
	for segment := range strings.SplitSeq(filepath.Clean(resolvedPath), string(filepath.Separator)) {
		if pciRootRegexp.MatchString(segment) {
			return segment, nil
		}
	}
	return "", nil
}
