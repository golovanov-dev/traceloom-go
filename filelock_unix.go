//go:build unix

package traceloom

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// acquireFileLock takes a kernel advisory lock on the whole file. The kernel releases it
// when the process dies, so a crashed writer cannot strand the lock and no staleness
// heuristic is needed.
func acquireFileLock(ctx context.Context, file *os.File) (unlockFunc, error) {
	bounded, cancel := boundedLockContext(ctx)
	defer cancel()

	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() error {
				return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}

		timer := time.NewTimer(lockRetryInterval)
		select {
		case <-bounded.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, bounded.Err()
		case <-timer.C:
		}
	}
}
