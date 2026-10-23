package nsexec

import (
	"errors"
	"fmt"
	"strings"
)

const Unshare = "unshare"

type Bind struct {
	Source string
	Target string
}

type Config struct {
	Hostname string
	Binds    []Bind
	Argv     []string
}

var ErrNoTarget = errors.New("nsexec: no command after --")

func ParseBinds(raw string) ([]Bind, error) {
	var out []Bind
	for b := range strings.SplitSeq(raw, ",") {
		b = strings.TrimSpace(b)
		if b == "" {
			continue
		}
		src, dst, ok := strings.Cut(b, ":")
		if !ok || !strings.HasPrefix(src, "/") || !strings.HasPrefix(dst, "/") {
			return nil, fmt.Errorf("nsexec: bind %q is not <absolute source>:<absolute target>", b)
		}
		out = append(out, Bind{Source: src, Target: dst})
	}
	return out, nil
}

func FormatBinds(b []Bind) string {
	parts := make([]string, 0, len(b))
	for _, x := range b {
		parts = append(parts, x.Source+":"+x.Target)
	}
	return strings.Join(parts, ",")
}

func (c Config) Validate() error {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return ErrNoTarget
	}
	if c.Hostname == "" || len(c.Hostname) > 64 {
		return fmt.Errorf("nsexec: the private hostname %q must be 1 to 64 bytes", c.Hostname)
	}
	return nil
}

func Wrap(self []string, hostname string, binds []Bind, argv []string) []string {
	out := append(append([]string(nil), self...), "--private-hostname", hostname)
	if len(binds) > 0 {
		out = append(out, "--bind", FormatBinds(binds))
	}
	return append(append(out, "--"), argv...)
}
