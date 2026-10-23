package debounce

import (
	"sync"
	"time"
)

const DefaultDelay = 5 * time.Second

type Debounce struct {
	delay time.Duration

	fn func()

	mu sync.Mutex

	timer *time.Timer

	stopped bool
}

func New(delay time.Duration, fn func()) *Debounce {
	if delay <= 0 {
		delay = DefaultDelay
	}
	return &Debounce{delay: delay, fn: fn}
}

func (d *Debounce) Mark() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped || d.timer != nil {
		return
	}
	d.timer = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		d.timer = nil
		stopped := d.stopped
		d.mu.Unlock()
		if !stopped {
			d.fn()
		}
	})
}

func (d *Debounce) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}
