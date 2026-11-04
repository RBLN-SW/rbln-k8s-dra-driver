package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rbln-sw/rblnlib-go/pkg/rblnsmi"
	"github.com/rbln-sw/rblnlib-go/pkg/rsdgroup"
)

func recreateValidatedRSDGroup(ctx context.Context, busIDs []string) (string, error) {
	// Keep this guard while go.mod pins v0.3.0. Once the library's own
	// membership validation is released, use RecreateRsdGroup directly.
	path, err := rsdgroup.RecreateRsdGroup(busIDs)
	if err != nil {
		return "", err
	}
	// v0.3.0 may omit unmatched PCI IDs yet report successful creation.
	// Confirm the actual group before exposing any of its device nodes.
	info, err := readGroupedInventory(ctx)
	if err != nil {
		return "", err
	}
	if err := validateRSDMembership(path, busIDs, info); err != nil {
		return "", err
	}
	return path, nil
}

func readGroupedInventory(ctx context.Context) (*rblnsmi.RblnSmi, error) {
	// Match rblnlib-go's executable precedence. A container may use the
	// host-installed binary or a mounted driver rootfs.
	command := ""
	for _, path := range []string{"/usr/bin/rbln-smi", "/run/rbln/driver/usr/bin/rbln-smi", "/host/usr/bin/rbln-smi", "/host/driver/usr/bin/rbln-smi"} {
		if fi, err := os.Stat(path); err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
			command = path
			break
		}
	}
	if command == "" {
		var err error
		command, err = exec.LookPath("rbln-smi")
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, command, "-g", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("read RSD group membership: %w", err)
	}
	var info rblnsmi.RblnSmi
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("decode RSD group membership: %w", err)
	}
	return &info, nil
}

func validateRSDMembership(path string, expected []string, info *rblnsmi.RblnSmi) error {
	group, ok := strings.CutPrefix(path, "/dev/rsd")
	id, err := strconv.Atoi(group)
	if !ok || err != nil || id <= 0 || group != strconv.Itoa(id) {
		return fmt.Errorf("invalid private RSD group path %q", path)
	}
	wanted := map[string]bool{}
	for _, bus := range expected {
		wanted[strings.ToLower(bus)] = true
	}
	if len(wanted) == 0 {
		return fmt.Errorf("RSD group %s has no requested devices", group)
	}
	seen := map[string]bool{}
	for _, d := range info.Devices {
		if d.GroupID != group {
			continue
		}
		bus := strings.ToLower(d.PCI.BusID)
		if !wanted[bus] || seen[bus] {
			return fmt.Errorf("RSD group %s contains unexpected or duplicate device %s", group, d.PCI.BusID)
		}
		seen[bus] = true
	}
	if len(seen) != len(wanted) {
		return fmt.Errorf("RSD group %s contains %d of %d requested devices", group, len(seen), len(wanted))
	}
	return nil
}
