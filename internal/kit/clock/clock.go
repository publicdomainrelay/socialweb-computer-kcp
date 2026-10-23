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

func DurationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
}

func SecondsBetween(a, b time.Time) float64 { return float64(b.Sub(a).Milliseconds()) / 1000 }

func NowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
