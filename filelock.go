package traceloom

import (
	"context"
	"os"
	"time"
)

const (
	lockWaitTimeout   = 10 * time.Second
	lockRetryInterval = 5 * time.Millisecond
)

type unlockFunc func() error

func acquirePathLock(ctx context.Context, path string, mode os.FileMode) (unlockFunc, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, mode)
	if err != nil {
		return nil, err
	}
	unlockFile, err := acquireFileLock(ctx, file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() error {
		unlockErr := unlockFile()
		closeErr := file.Close()
		if unlockErr != nil {
			return unlockErr
		}
		return closeErr
	}, nil
}

func boundedLockContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, lockWaitTimeout)
}
