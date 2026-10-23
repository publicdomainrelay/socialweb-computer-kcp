package childproc

import (
	"bytes"
	"io"
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

var irohSecretLine = []byte("using secret key")

type SecretFilter struct {
	mu sync.Mutex

	out io.Writer

	start bool

	dropping bool

	pending []byte
}

func FilterSecrets(out io.Writer) *SecretFilter {
	if out == nil {
		out = io.Discard
	}
	return &SecretFilter{out: out, start: true}
}

func (f *SecretFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for _, c := range p {
		switch {
		case f.dropping:
			if c == '\n' {
				f.dropping, f.start = false, true
			}
		case f.start || len(f.pending) > 0:
			f.pending = append(f.pending, c)
			switch {
			case bytes.HasPrefix(f.pending, irohSecretLine):
				f.dropping, f.start, f.pending = c != '\n', c == '\n', f.pending[:0]
			case bytes.HasPrefix(irohSecretLine, f.pending):
				f.start = false
			default:
				out = append(out, f.pending...)
				f.pending, f.start = f.pending[:0], c == '\n'
			}
		default:
			out = append(out, c)
			f.start = c == '\n'
		}
	}
	if len(out) > 0 {
		if _, err := f.out.Write(out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
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
