package rblnlib

import (
	"context"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func acquireFileLock(ctx context.Context, lockPath string) (release func() error, err error) {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, lockFilePerm)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	unlock := func() error {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		return f.Close()
	}

	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("acquire lock timeout/canceled: %w", ctx.Err())
		default:
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				<-ticker.C
				continue
			}
			return unlock, nil
		}
	}
}
