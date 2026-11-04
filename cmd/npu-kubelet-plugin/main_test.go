package main

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/urfave/cli/v2"
)

// The app must build its flag set and render help without the deleted
// component-base logging flags.
func TestNewAppRunsHelp(t *testing.T) {
	app := newApp()
	app.Writer = io.Discard
	if err := app.Run([]string{"npu-kubelet-plugin", "--help"}); err != nil {
		t.Fatalf("--help: %v", err)
	}
}

func TestDeviceScanIntervalCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		env  map[string]string
		want time.Duration
	}{
		{name: "default", want: 5 * time.Second},
		{name: "legacy flag", args: []string{"--vfio-rescan-interval=30s"}, want: 30 * time.Second},
		{name: "legacy flag disables scanning", args: []string{"--vfio-rescan-interval=0"}, want: 0},
		{name: "legacy environment", env: map[string]string{"VFIO_RESCAN_INTERVAL": "30s"}, want: 30 * time.Second},
		{name: "legacy environment disables scanning", env: map[string]string{"VFIO_RESCAN_INTERVAL": "0"}, want: 0},
		{name: "new environment wins", env: map[string]string{"DEVICE_SCAN_INTERVAL": "10s", "VFIO_RESCAN_INTERVAL": "30s"}, want: 10 * time.Second},
		{name: "new flag overrides legacy environment", args: []string{"--device-scan-interval=15s"}, env: map[string]string{"VFIO_RESCAN_INTERVAL": "30s"}, want: 15 * time.Second},
		{name: "legacy flag overrides new environment", args: []string{"--vfio-rescan-interval=15s"}, env: map[string]string{"DEVICE_SCAN_INTERVAL": "10s"}, want: 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"DEVICE_SCAN_INTERVAL", "VFIO_RESCAN_INTERVAL"} {
				t.Setenv(name, "")
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			app := newApp()
			app.Writer = io.Discard
			app.Action = func(c *cli.Context) error {
				if got := c.Duration("device-scan-interval"); got != tc.want {
					t.Errorf("scan interval = %v, want %v", got, tc.want)
				}
				return nil
			}
			if err := app.Run(append([]string{"npu-kubelet-plugin", "--node-name=test-node"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
