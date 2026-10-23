package hostos

import (
	"context"
	"errors"
	"os"
	"time"
)

const (
	lockRetryMin = 10 * time.Millisecond

	lockRetryMax = 500 * time.Millisecond
)

var ErrLockHeld = errors.New("hostos: the lock stayed held")

// LockWait takes the lock on f without blocking in the kernel: it retries a
// non-blocking lock with backoff until it holds the lock, wait passes or ctx
// ends.
func LockWait(ctx context.Context, f *os.File, how int, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	delay := lockRetryMin
	for {
		err := Lock(f, how|LockNonBlocking)
		if err == nil {
			return nil
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return errors.Join(ErrLockHeld, err)
			}
			return errors.Join(ctx.Err(), err)
		case <-t.C:
		}
		delay = min(delay*2, lockRetryMax)
	}
}
