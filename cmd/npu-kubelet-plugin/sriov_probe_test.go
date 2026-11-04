package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSRIOVAttr(t *testing.T, root, busID, value string) {
	t.Helper()
	dir := filepath.Join(root, busID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
}

func linkSRIOVVF(t *testing.T, root, vf, pf string) {
	t.Helper()
	writeSRIOVAttr(t, root, vf, "")
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
			got, err := probeSRIOV(root, busID)
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
			if _, err := probeSRIOV(root, busID); err == nil {
				t.Fatal("incomplete sysfs must fail closed")
			}
		})
	}
}
