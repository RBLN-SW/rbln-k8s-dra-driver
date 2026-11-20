package main

import (
	"slices"
	"testing"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/rsdview"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func TestSelectionBindsAllAndOnlySelectedEntries(t *testing.T) {
	a, b := "0000:05:00.0", "0000:06:00.0"
	s := rsdview.State{Device: unix.Mkdev(240, 7), Full: []string{a, b}}
	cfg := specs.Spec{Linux: &specs.Linux{Devices: []specs.LinuxDevice{{Path: "/dev/rsd0", Type: "c", Major: 240, Minor: 7}}}, Hooks: &specs.Hooks{}}
	h := func(uid, bus string) specs.Hook {
		return specs.Hook{Path: rsdview.Hook, Args: []string{rsdview.Hook, uid, bus}}
	}
	for _, tc := range []struct {
		name  string
		hooks []specs.Hook
		want  []string
	}{
		{"subset", []specs.Hook{h("claim", b)}, []string{b}},
		{"full", []specs.Hook{h("claim", b), h("claim", a)}, []string{a, b}},
		{"different claims", []specs.Hook{h("claim", b), h("other", a)}, nil},
		{"not allocated", []specs.Hook{h("claim", "0000:07:00.0")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg.Hooks.CreateRuntime = tc.hooks
			got, err := selection(&cfg, "claim", b, s)
			if (err != nil) != (tc.want == nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, %v", got, err)
			}
		})
	}
}
