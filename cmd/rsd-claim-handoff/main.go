// The OCI runtime calls this static hook in its own namespace after creating
// the container process and before starting its workload.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/rsdview"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	system "github.com/rbln-sw/rblnlib-go/pkg/system/v1"
	"golang.org/x/sys/unix"
)

func processStart(pid int) (string, error) {
	if pid <= 1 {
		return "", errors.New("invalid OCI container PID")
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// comm is parenthesized and can contain spaces or closing parentheses.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("invalid proc stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", errors.New("missing proc starttime")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return "", nil
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	return fields[19], nil
}

// Every selected native CDI entry contributes one hook. The complete bundle
// is available even to the first invocation, including selections from a
// ResourceClaim's named requests. Repeated hooks are idempotent.
func selection(cfg *specs.Spec, uid, pci string, s rsdview.State) ([]string, error) {
	if cfg.Hooks == nil || cfg.Linux == nil {
		return nil, errors.New("missing OCI Linux devices or hooks")
	}
	var want []string
	for _, h := range cfg.Hooks.CreateRuntime {
		if h.Path != rsdview.Hook {
			continue
		}
		if len(h.Args) != 3 || h.Args[1] != uid {
			return nil, errors.New("multiple RSD claims cannot share one container RSD endpoint")
		}
		a, err := system.NormalizePCIAddress(h.Args[2])
		if err != nil {
			return nil, err
		}
		if !slices.Contains(s.Full, a) {
			return nil, errors.New("OCI request does not belong to the prepared claim")
		}
		if !slices.Contains(want, a) {
			want = append(want, a)
		}
	}
	a, err := system.NormalizePCIAddress(pci)
	if err != nil || !slices.Contains(want, a) {
		return nil, errors.New("current CDI request absent from OCI bundle")
	}
	var valid bool
	for _, d := range cfg.Linux.Devices {
		if d.Path == "/dev/rsd0" {
			if d.Type != "c" || d.Major != int64(unix.Major(s.Device)) || d.Minor != int64(unix.Minor(s.Device)) {
				return nil, errors.New("OCI RSD endpoint differs from prepared claim")
			}
			valid = true
		}
	}
	if !valid {
		return nil, errors.New("OCI RSD endpoint missing")
	}
	slices.Sort(want)
	return want, nil
}

var errBusy = errors.New("live RSD consumer requires a different device set")

func retainLiveLeases(leases map[string]rsdview.Lease, want []string, id string, pid int, start string, started func(int) (string, error)) error {
	for key, l := range leases {
		actual, err := started(l.PID)
		if err != nil {
			return err
		}
		if actual == "" || actual != l.Start {
			delete(leases, key)
			continue
		}
		if !slices.Equal(want, l.Members) {
			return errBusy
		}
		if key == id && (pid != l.PID || start != l.Start) {
			return errors.New("OCI container ID collision")
		}
	}
	return nil
}

func (p *probe) transact(ctx context.Context, s *rsdview.State, want []string, id string, pid int, start string) error {
	if err := retainLiveLeases(s.Leases, want, id, pid, start, processStart); err != nil {
		if errors.Is(err, errBusy) && s.Waiting != id {
			s.Waiting = id
			if saveErr := s.Save(); saveErr != nil {
				return saveErr
			}
		}
		return err
	}
	inv, err := p.read(ctx)
	if err != nil {
		return err
	}
	current := members(inv, s.Group)
	interrupted := len(s.Pending) > 0 && (len(current) == 0 || slices.Equal(current, s.Pending))
	if !slices.Equal(current, s.Current) && !interrupted {
		return errors.New("unexpected group membership; refusing to modify the slot")
	}
	if len(s.Leases) > 0 && !slices.Equal(current, want) {
		return errors.New("live consumer group no longer matches its lease")
	}
	for _, addr := range want {
		found := false
		for _, d := range inv {
			if d.PCI.Address == addr && (d.Group.String() == "0" || d.Group.String() == s.Group) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("claim member unavailable: %s", addr)
		}
	}
	if !slices.Equal(current, want) {
		if len(current) > 0 {
			number, err := rdev(s.Group)
			if err != nil {
				return err
			}
			if number != s.Device {
				return errors.New("RSD number changed before transition")
			}
		}
		// Record both sides before the first SMI mutation. If interrupted after
		// deletion, only this claim's journal can authorize rebuilding an empty slot.
		if len(current) > 0 {
			s.Current = slices.Clone(current)
		}
		s.Pending = slices.Clone(want)
		if err := s.Save(); err != nil {
			return err
		}
		if len(current) > 0 {
			if _, err := p.command(ctx, "group", "-d", s.Group); err != nil {
				return err
			}
		}
		if err := p.create(ctx, s.Group, want); err != nil {
			if len(current) == 0 {
				return err
			}
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			return errors.Join(err, p.create(recovery, s.Group, current))
		}
	}
	readback, err := p.read(ctx)
	if err != nil {
		return err
	}
	if !slices.Equal(members(readback, s.Group), want) {
		return errors.New("RSD membership readback failed")
	}
	number, err := rdev(s.Group)
	if err != nil {
		return err
	}
	if number != s.Device {
		return errors.New("RSD number changed after OCI device resolution")
	}
	s.Current = slices.Clone(want)
	if s.Waiting == id {
		s.Waiting = ""
	}
	s.Pending = nil
	s.Leases[id] = rsdview.Lease{PID: pid, Start: start, Members: want}
	return s.Save()
}

func run(ctx context.Context, args []string, input io.Reader) error {
	if len(args) != 2 {
		return errors.New("expected claim UID and PCI address")
	}
	var state specs.State
	if err := json.NewDecoder(io.LimitReader(input, 1<<20)).Decode(&state); err != nil {
		return err
	}
	if state.ID == "" || !filepath.IsAbs(state.Bundle) {
		return errors.New("invalid OCI state")
	}
	start, err := processStart(state.Pid)
	if err != nil {
		return err
	}
	if start == "" {
		return errors.New("container process unavailable")
	}
	f, err := os.Open(filepath.Join(state.Bundle, "config.json"))
	if err != nil {
		return err
	}
	defer f.Close()
	var cfg specs.Spec
	if err = json.NewDecoder(f).Decode(&cfg); err != nil {
		return err
	}
	// A runtime can surface a hook error as a terminal StartError for a
	// restartPolicy Never Pod. Wait for short-lived claim contention before
	// returning an error, and let unrelated transactions take the node lock.
	for {
		err = attempt(ctx, args, state, start, &cfg)
		if !errors.Is(err, errBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func attempt(ctx context.Context, args []string, state specs.State, start string, cfg *specs.Spec) error {
	lock, err := os.OpenFile(rsdview.Lock, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
	s, err := rsdview.Load(args[0])
	if err != nil {
		return err
	}
	want, err := selection(cfg, args[0], args[1], s)
	if err != nil {
		return err
	}
	for _, path := range []string{"/usr/bin/rbln-smi", "/run/rbln/driver/usr/bin/rbln-smi"} {
		if f, err := os.Stat(path); err == nil && f.Mode().IsRegular() && f.Mode().Perm()&0111 != 0 {
			p := probe{driverRoot: path == "/run/rbln/driver/usr/bin/rbln-smi"}
			return p.transact(ctx, &s, want, state.ID, state.Pid, start)
		}
	}
	return errors.New("host SMI unavailable")
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "install" {
		if err := rsdview.InstallHook(); err != nil {
			log.Fatal(err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	err := run(ctx, os.Args[1:], os.Stdin)
	cancel()
	if err != nil {
		if len(os.Args) == 3 {
			if path, e := rsdview.Path(os.Args[1]); e == nil {
				if _, e = os.Stat(path); e == nil {
					if logErr := os.WriteFile(path+".error", []byte(err.Error()), 0600); logErr != nil {
						log.Print(logErr)
					}
				}
			}
		}
		log.Fatal(err)
	}
}
