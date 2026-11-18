package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeSRIOVAttr(t *testing.T, root, busID, value string) {
	t.Helper()
	dir := filepath.Join(root, busID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"class": npuPCIClassID, "device": "0x2130"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "drivers/rebellions"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor"), []byte(rblnPCIVendorID), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../drivers/rebellions", filepath.Join(dir, "driver")); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if value != "" {
		if err := os.WriteFile(filepath.Join(dir, "sriov_numvfs"), []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n >= 0 {
		completeSRIOVFixture(t, root, busID, n)
	}

}

func linkSRIOVVF(t *testing.T, root, vf, pf string) {
	t.Helper()
	writeSRIOVAttr(t, root, vf, "")
	if err := os.Remove(filepath.Join(root, vf, "physfn")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink("../"+pf, filepath.Join(root, vf, "physfn")); err != nil {
		t.Fatal(err)
	}
}

func TestProbeSRIOV(t *testing.T) {
	for _, tc := range []struct {
		name, count string
		vf, wantErr bool
		want        sriovInfo
	}{
		{name: "legacy PF"},
		{name: "unpartitioned PF", count: "0\n"},
		{name: "hosting PF with one VF", count: "1", want: sriovInfo{numVFs: 1}},
		{name: "hosting PF", count: "4", want: sriovInfo{numVFs: 4}},
		{name: "unsupported PF mode", count: "2", wantErr: true},
		{name: "vf1", count: "1", vf: true, want: sriovInfo{parentPCIBusID: "0000:17:00.0", numVFs: 1}},
		{name: "vf4", count: "4\n", vf: true, want: sriovInfo{parentPCIBusID: "0000:17:00.0", numVFs: 4}},
		{name: "unsupported mode", count: "2", vf: true, wantErr: true},
		{name: "zero VF count", count: "0", vf: true, wantErr: true},
		{name: "missing parent count", vf: true, wantErr: true},
		{name: "negative count", count: "-1", wantErr: true},
		{name: "malformed count", count: "garbled", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			busID := "0000:17:00.0"
			writeSRIOVAttr(t, root, busID, tc.count)
			if tc.vf {
				linkSRIOVVF(t, root, "0000:17:00.1", busID)
				busID = "0000:17:00.1"
			}
			got, err := probeSRIOV(t.Context(), root, busID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("probe = %+v, %v", got, err)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestProbeSRIOVRejectsIncompleteSysfs(t *testing.T) {
	for _, tc := range []string{"missing device", "missing parent", "invalid parent", "not a symlink"} {
		t.Run(tc, func(t *testing.T) {
			root := t.TempDir()
			busID := "0000:17:00.1"
			switch tc {
			case "missing parent":
				linkSRIOVVF(t, root, busID, "0000:17:00.0")
			case "invalid parent":
				linkSRIOVVF(t, root, busID, "not-a-pci-address")
			case "not a symlink":
				writeSRIOVAttr(t, root, busID, "")
				if err := os.WriteFile(filepath.Join(root, busID, "physfn"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := probeSRIOV(t.Context(), root, busID); err == nil {
				t.Fatal("incomplete sysfs must fail closed")
			}
		})
	}
}

// Complete the kernel topology when a test changes a partition profile.
func completeSRIOVFixture(t *testing.T, root, pf string, count int) {
	t.Helper()
	links, err := filepath.Glob(filepath.Join(root, pf, "virtfn*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range links {
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < count; i++ {
		vf := strings.TrimSuffix(pf, ".0") + fmt.Sprintf(".%d", i+1)
		path := filepath.Join(root, vf)
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
		for attr, value := range map[string]string{"vendor": rblnPCIVendorID, "class": npuPCIClassID, "device": "0x2131"} {
			file := filepath.Join(path, attr)
			if _, err := os.Stat(file); os.IsNotExist(err) {
				if err := os.WriteFile(file, []byte(value), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
		parent := filepath.Join(path, "physfn")
		if err := os.Remove(parent); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, pf), parent); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(root, pf, fmt.Sprintf("virtfn%d", i))); err != nil {
			t.Fatal(err)
		}
	}
}
