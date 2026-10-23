package childproc

import (
	"os/exec"
	"sync"
	"time"
)

const TailLimit = 2048

type Tail struct {
	mu sync.Mutex

	buf []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > TailLimit {
		t.buf = t.buf[len(t.buf)-TailLimit:]
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func Kill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func NextBackoff(current, minimum, maximum time.Duration) time.Duration {
	if current <= 0 {
		return minimum
	}
	return min(current*2, maximum)
}
