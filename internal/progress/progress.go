package progress

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
)

const (
	DefaultInterval = 5 * time.Second
	MinBytes        = 8 << 20
	barWidth        = 28
)

func IsTTYWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	div, i := float64(unit), 0
	for v := float64(n) / unit; v >= unit && i < len(units)-1; v /= unit {
		div *= unit
		i++
	}
	return fmt.Sprintf("%.2f %s", float64(n)/div, units[i])
}

func Bar(done, total int64, width int) string {
	if total <= 0 {
		return Bytes(done)
	}
	frac := float64(done) / float64(total)
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	return fmt.Sprintf("[%s%s] %3d%% %s/%s", strings.Repeat("#", filled),
		strings.Repeat(" ", width-filled), int(frac*100), Bytes(done), Bytes(total))
}

func Line(name string, done, total int64, tty bool) string {
	if tty {
		return name + " " + Bar(done, total, barWidth)
	}
	return fmt.Sprintf("fetching %s: %s/%s", name, Bytes(done), Bytes(total))
}

func Elapsed(d time.Duration) string { return d.Round(time.Second).String() }

type Progress struct {
	name     string
	total    int64
	out      io.Writer
	tty      bool
	every    time.Duration
	done     int64
	rendered int64
	last     time.Time
	quiet    bool
	finished bool
	lastLine string
}

func New(name string, total int64, out io.Writer, tty bool, every time.Duration) *Progress {
	every = opt.Positive(every, DefaultInterval)
	return &Progress{name: name, total: total, out: out, tty: tty, every: every, last: time.Now(),
		quiet: out == nil || total < MinBytes}
}

func (p *Progress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.quiet {
		return len(b), nil
	}
	if now := time.Now(); now.Sub(p.last) >= p.every {
		p.last = now
		p.render()
	}
	return len(b), nil
}

func (p *Progress) Finish() {
	if p.quiet || p.finished {
		return
	}
	p.finished = true
	if p.rendered != p.done {
		p.render()
	}
	if p.tty {
		fmt.Fprintln(p.out)
	}
}

func (p *Progress) render() {
	p.rendered = p.done
	line := Line(p.name, p.done, p.total, p.tty)
	if p.tty {
		if n := len(p.lastLine) - len(line); n > 0 {
			line += strings.Repeat(" ", n)
		}
		p.lastLine = line
		fmt.Fprintf(p.out, "\r%s", line)
		return
	}
	fmt.Fprintln(p.out, line)
}
