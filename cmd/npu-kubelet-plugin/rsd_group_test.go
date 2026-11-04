package main

import (
	"testing"

	"github.com/rbln-sw/rblnlib-go/pkg/rblnsmi"
)

func TestRSDMembershipRequiresExactlyTheRequestedDevices(t *testing.T) {
	member := func(bus, group string) rblnsmi.Device {
		return rblnsmi.Device{GroupID: group, PCI: rblnsmi.PCIInfo{BusID: bus}}
	}
	expected := []string{"0000:0A:00.1", "0000:0A:00.2"}
	for _, tc := range []struct {
		name    string
		path    string
		devices []rblnsmi.Device
		wantErr bool
	}{
		{"complete", "/dev/rsd1", []rblnsmi.Device{member("0000:0a:00.2", "1"), member("0000:0a:00.1", "1"), member("0000:0b:00.1", "2")}, false},
		{"partial library success", "/dev/rsd1", []rblnsmi.Device{member("0000:0a:00.1", "1")}, true},
		{"extra foreign VF", "/dev/rsd1", []rblnsmi.Device{member("0000:0a:00.1", "1"), member("0000:0a:00.2", "1"), member("0000:0b:00.1", "1")}, true},
		{"wrong group", "/dev/rsd1", []rblnsmi.Device{member("0000:0a:00.1", "2"), member("0000:0a:00.2", "2")}, true},
		{"duplicate", "/dev/rsd1", []rblnsmi.Device{member("0000:0a:00.1", "1"), member("0000:0a:00.1", "1")}, true},
		{"host group forbidden", "/dev/rsd0", nil, true},
		{"empty result", "", nil, true},
		{"invalid path", "/dev/rsd../other", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRSDMembership(tc.path, expected, &rblnsmi.RblnSmi{Devices: tc.devices})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
