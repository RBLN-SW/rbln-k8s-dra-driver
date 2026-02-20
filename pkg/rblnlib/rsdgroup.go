package rblnlib

import (
	"context"
	"time"

	"github.com/RBLN-SW/k8s-dra-driver-npu/pkg/rblnsmi"
	"github.com/golang/glog"
)

const (
	rsdDevice          = "/dev/rsd"
	defaultRsdDevice   = rsdDevice + "0"
	lockPollInterval   = 100 * time.Millisecond
	lockFilePerm       = 0o644
	rsdGroupLockFile   = "/var/run/rbln-rsd-group.lock"
	defaultExecTimeout = 5 * time.Second
)

func RecreateRsdGroup(deviceIDs []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), defaultExecTimeout)
	defer cancel()

	groupID, err := withRsdLock(ctx, func() (string, error) {
		if err := rblnsmi.DestroyRsdGroup(ctx, deviceIDs); err != nil {
			glog.Errorf("Failed to destroy RSD groups: %q", err)
			return "", err
		}
		return rblnsmi.CreateRsdGroup(ctx, deviceIDs)
	})

	if err != nil {
		glog.Errorf("Failed to create RSD groups: %q", err)
		return defaultRsdDevice
	}
	return rsdDevice + groupID
}

func withRsdLock(ctx context.Context, fn func() (string, error)) (string, error) {
	release, err := acquireFileLock(ctx, rsdGroupLockFile)
	if err != nil {
		return "", err
	}
	defer func() {
		if relErr := release(); err == nil && relErr != nil {
			err = relErr
		}
	}()
	return fn()
}
