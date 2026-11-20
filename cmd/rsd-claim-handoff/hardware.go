package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"
	"golang.org/x/sys/unix"
)

type probe struct{ driverRoot bool }

type device struct {
	Name  string      `json:"device"`
	Group json.Number `json:"group_id"`
	Index json.Number `json:"npu"`
	PCI   struct {
		Address string `json:"bus_id"`
	} `json:"pci"`
}

func (p *probe) command(ctx context.Context, args ...string) ([]byte, error) {
	var cmd *exec.Cmd
	if p.driverRoot {
		cmd = exec.CommandContext(ctx, "/run/rbln/driver/usr/bin/rbln-smi", args...)
	} else {
		cmd = exec.CommandContext(ctx, "/usr/bin/rbln-smi", args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("SMI %q: %w: %.600s", args, err, out)
	}
	return out, nil
}
func (p *probe) read(ctx context.Context) ([]device, error) {
	raw, err := p.command(ctx, "-g", "-j")
	if err != nil {
		return nil, err
	}
	var d struct {
		Devices []device `json:"devices"`
	}
	if err = json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range d.Devices {
		normalized, err := system.NormalizePCIAddress(d.Devices[i].PCI.Address)
		if err != nil || seen[normalized] {
			return nil, errors.New("invalid SMI identity")
		}
		d.Devices[i].PCI.Address = normalized
		seen[normalized] = true
	}
	return d.Devices, nil
}
func members(inventory []device, group string) []string {
	var m []string
	for _, d := range inventory {
		if d.Group.String() == group {
			m = append(m, d.PCI.Address)
		}
	}
	slices.Sort(m)
	return m
}
func rdev(group string) (uint64, error) {
	id, err := strconv.ParseUint(group, 10, 32)
	if err != nil || id == 0 {
		return 0, errors.New("invalid private group ID")
	}
	path := fmt.Sprintf("/dev/rsd%d", id)
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return 0, errors.New("RSD path is not a character device")
	}
	return uint64(st.Rdev), nil
}

func (p *probe) create(ctx context.Context, group string, wanted []string) error {
	inv, err := p.read(ctx)
	if err != nil {
		return err
	}
	if len(members(inv, group)) != 0 {
		return errors.New("claim RSD slot is still occupied")
	}
	help, err := p.command(ctx, "group", "--help")
	if err != nil {
		return err
	}
	contract := strings.ToLower(string(help))
	usePCI := strings.Contains(contract, "pci address")
	if !usePCI && !strings.Contains(contract, "attach npu ids") {
		return errors.New("unsupported SMI selector syntax")
	}
	var selectors []string
	for _, addr := range wanted {
		found := false
		for _, d := range inv {
			if d.PCI.Address != addr {
				continue
			}
			if d.Group.String() != "0" {
				return errors.New("claim member belongs to another RSD slot")
			}
			v := addr
			if !usePCI {
				i, err := d.Index.Int64()
				if err != nil || i < 0 {
					return errors.New("invalid default-group index")
				}
				v = strconv.FormatInt(i, 10)
			}
			selectors = append(selectors, v)
			found = true
			break
		}
		if !found {
			return fmt.Errorf("claim member disappeared: %s", addr)
		}
	}
	_, err = p.command(ctx, "group", "-c", group, "-a", strings.Join(selectors, ","))
	return err
}
