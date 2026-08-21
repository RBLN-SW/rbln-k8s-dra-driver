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
	"testing"
)

func TestResolvePCIERootID(t *testing.T) {
	// sysfs represents the PCI domain as four hex digits, so a
	// non-decimal domain like 000a must resolve too.
	cases := []struct {
		domain string
		busID  string
		want   string
	}{
		{domain: "0000", busID: "0000:27:00.0", want: "pci0000:00"},
		{domain: "000a", busID: "000a:27:00.0", want: "pci000a:00"},
	}

	for _, tc := range cases {
		t.Run(tc.domain, func(t *testing.T) {
			dir := t.TempDir()
			devDir := filepath.Join(dir, "sys", "devices", "pci"+tc.domain+":00", tc.domain+":00:01.0", tc.busID)
			if err := os.MkdirAll(devDir, 0o755); err != nil {
				t.Fatal(err)
			}
			busDir := filepath.Join(dir, "sys", "bus", "pci", "devices")
			if err := os.MkdirAll(busDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(devDir, filepath.Join(busDir, tc.busID)); err != nil {
				t.Fatal(err)
			}

			got, err := resolvePCIERootID(busDir, tc.busID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("resolvePCIERootID(%s) = %q, want %q", tc.busID, got, tc.want)
			}
		})
	}
}
