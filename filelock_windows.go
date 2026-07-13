//go:build windows

package traceloom

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// LockFileEx and UnlockFileEx are resolved from kernel32 rather than imported from
// golang.org/x/sys, which keeps the package free of dependencies.
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	// A range wide enough to cover any shard, so the lock behaves like a whole-file lock.
	lockRangeLow  = 0xFFFFFFFF
	lockRangeHigh = 0xFFFFFFFF
)

// acquireFileLock takes a Windows byte-range lock on the whole file. Like flock, the
// kernel releases it when the handle closes, including on process death.
func acquireFileLock(ctx context.Context, file *os.File) (unlockFunc, error) {
	bounded, cancel := boundedLockContext(ctx)
	defer cancel()

	for {
		var overlapped syscall.Overlapped
		result, _, err := procLockFileEx.Call(
			file.Fd(),
			uintptr(lockfileExclusiveLock|lockfileFailImmediately),
			0,
			lockRangeLow,
			lockRangeHigh,
			uintptr(unsafe.Pointer(&overlapped)),
		)
		if result != 0 {
			return func() error { return unlockFile(file) }, nil
		}
		if !errors.Is(err, errorLockViolation) && !errors.Is(err, errorIOPending) {
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

// Reported when the range is already held and the request would otherwise block.
const (
	errorLockViolation = syscall.Errno(33)
	errorIOPending     = syscall.Errno(997)
)

func unlockFile(file *os.File) error {
	var overlapped syscall.Overlapped
	result, _, err := procUnlockFileEx.Call(
		file.Fd(),
		0,
		lockRangeLow,
		lockRangeHigh,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if result == 0 {
		return err
	}
	return nil
}
