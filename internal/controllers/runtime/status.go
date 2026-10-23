package runtime

import "time"

func Period(configured, fallback time.Duration) time.Duration {
	if configured == 0 {
		return fallback
	}
	return configured
}

func RefreshAfter(period time.Duration) Result {
	if period <= 0 {
		return Result{}
	}
	return Result{RequeueAfter: period}
}
