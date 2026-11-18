package main

import (
	"context"
	"path/filepath"

	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"
)

func fixtureNativeNodes(st *DeviceState) func(context.Context, string) ([]system.DeviceNode, error) {
	return func(_ context.Context, address string) ([]system.DeviceNode, error) {
		for _, d := range st.allocatable {
			bus, err := devicePCIBusID(d)
			if err != nil || bus != address {
				continue
			}
			path := filepath.Join("/dev", deviceStringAttr(d, deviceNodeNameAttributeKey))
			return []system.DeviceNode{{Path: path, HostPath: path, Kind: "native"}}, nil
		}
		return nil, system.ErrNotFound
	}
}

const (
	rblnPCIVendorID = "0x1eff"
	npuPCIClassID   = "0x120000"
)
