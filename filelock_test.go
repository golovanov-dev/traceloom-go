package traceloom

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The kernel lock is exclusive across handles, and waiting for it honors the context.
// Both platforms release it on process death, so no staleness heuristic is needed.
func TestFileLockIsExclusiveAndHonorsContextDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".traceloom.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	unlock, err := acquireFileLock(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := acquireFileLock(ctx, second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock must block a second handle: %v", err)
	}

	if err := unlock(); err != nil {
		t.Fatal(err)
	}

	released, err := acquireFileLock(context.Background(), second)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if err := released(); err != nil {
		t.Fatal(err)
	}
}
