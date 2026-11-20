// Package rsdview stores the identity assigned by NodePrepare and the OCI
// process leases for an explicitly enabled native claim.
package rsdview

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"
)

const (
	Root       = "/run/rbln/rsd-view-v1"
	Hook       = Root + "/verify"
	Lock       = "/dev/.rbln-rsd-writer.lock"
	Annotation = "npu.rebellions.ai/rsd-request-view"
)

type Lease struct {
	PID     int
	Start   string
	Members []string
}
type State struct {
	UID, Boot, Group       string
	Device                 uint64
	Full, Current, Pending []string
	Leases                 map[string]Lease
}

func Path(uid string) (string, error) {
	if _, err := uuid.Parse(uid); err != nil {
		return "", fmt.Errorf("invalid claim UID: %w", err)
	}
	return filepath.Join(Root, uid+".json"), nil
}

func BootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b)), err
}

func atomicFile(path string, mode os.FileMode, write func(*os.File) error) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".stage-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = errors.Join(f.Chmod(mode), write(f), f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func Load(uid string) (State, error) {
	var s State
	path, err := Path(uid)
	if err != nil {
		return s, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	boot, err := BootID()
	if err != nil {
		return s, err
	}
	if s.UID != uid || s.Boot != boot || s.Group == "" || s.Group == "0" || s.Device == 0 || len(s.Full) == 0 || s.Leases == nil {
		return s, errors.New("invalid or stale prepared RSD claim")
	}
	return s, nil
}

func (s State) Save() error {
	path, err := Path(s.UID)
	if err != nil {
		return err
	}
	return atomicFile(path, 0600, func(f *os.File) error { return json.NewEncoder(f).Encode(s) })
}

// Called by the kubelet driver while preparing an opted-in native claim.
// The hook is a static host executable, independent of driver Pod lifetime.
func Install(s State) error {
	if err := os.MkdirAll(Root, 0700); err != nil {
		return err
	}
	fi, err := os.Lstat(Root)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0700 {
		return errors.New("invalid RSD runtime state directory")
	}
	binary, err := os.Open("/usr/bin/rsd-claim-handoff")
	if err != nil {
		return err
	}
	defer binary.Close()
	if err = atomicFile(Hook, 0755, func(f *os.File) error { _, err := io.Copy(f, binary); return err }); err != nil {
		return err
	}
	existing, err := Load(s.UID)
	if err == nil {
		if existing.Group == s.Group && existing.Device == s.Device && slices.Equal(existing.Full, s.Full) {
			return nil
		}
		return errors.New("prepared RSD identity changed")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.Boot, err = BootID()
	if err != nil {
		return err
	}
	s.Current = slices.Clone(s.Full)
	s.Leases = map[string]Lease{}
	return s.Save()
}

func Remove(uid string) error {
	path, err := Path(uid)
	if err != nil {
		return err
	}
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
