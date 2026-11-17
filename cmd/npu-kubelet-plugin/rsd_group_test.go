package main

import (
	"testing"

	smi "github.com/rbln-sw/rblnlib-go/pkg/smi/v1"
)

func TestRSDMembershipRequiresExactlyTheRequestedDevices(t *testing.T) {
	expected := []string{"0000:0A:00.1", "0000:0A:00.2"}
	for _, tc := range []struct {
		name    string
		id      uint32
		groups  []smi.Group
		wantErr bool
	}{
		{"complete", 1, []smi.Group{{ID: 1, PCIAddresses: []string{"0000:0a:00.2", "0000:0a:00.1"}}, {ID: 2, PCIAddresses: []string{"0000:0b:00.1"}}}, false},
		{"partial", 1, []smi.Group{{ID: 1, PCIAddresses: []string{"0000:0a:00.1"}}}, true},
		{"extra foreign VF", 1, []smi.Group{{ID: 1, PCIAddresses: []string{"0000:0a:00.1", "0000:0a:00.2", "0000:0b:00.1"}}}, true},
		{"wrong group", 1, []smi.Group{{ID: 2, PCIAddresses: expected}}, true},
		{"duplicate", 1, []smi.Group{{ID: 1, PCIAddresses: []string{"0000:0a:00.1", "0000:0a:00.1"}}}, true},
		{"default forbidden", 0, nil, true}, {"empty result", 1, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := smi.ValidateGroupMembers(tc.id, expected, tc.groups)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
