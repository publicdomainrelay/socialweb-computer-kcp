package clock

import (
	"context"
	"time"
)

func Sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func SleepErr(ctx context.Context, d time.Duration) error {
	if !Sleep(ctx, d) {
		return ctx.Err()
	}
	return nil
}
