package systemdunit

import (
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

type RestartPolicy string

const (
	RestartNo RestartPolicy = "no"

	RestartOnFailure RestartPolicy = "on-failure"

	RestartAlways RestartPolicy = "always"
)

func ShouldRestart(policy RestartPolicy, exitCode int) bool {
	switch policy {
	case RestartAlways:
		return true
	case RestartOnFailure:
		return exitCode != 0
	}
	return false
}

type Backoff struct {
	Min time.Duration

	Max time.Duration

	attempt int
}

func (b *Backoff) Next() time.Duration {
	minimum := b.Min
	minimum = opt.Positive(minimum, time.Second)
	maximum := b.Max
	maximum = opt.Positive(maximum, 30*time.Second)
	if maximum < minimum {
		maximum = minimum
	}
	b.attempt++
	delay := minimum
	for i := 1; i < b.attempt && delay < maximum; i++ {
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	return delay
}

func (b *Backoff) Reset() { b.attempt = 0 }

func RestartDelay(attempt int, minimum, maximum time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	minimum = opt.Positive(minimum, time.Second)
	maximum = opt.Positive(maximum, 30*time.Second)
	if maximum < minimum {
		maximum = minimum
	}
	delay := minimum
	for i := 1; i < attempt && delay < maximum; i++ {
		delay *= 2
	}
	if delay > maximum {
		delay = maximum
	}
	return delay
}
