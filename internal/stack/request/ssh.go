package request

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/childproc"
)

const (
	KeyFile          = "id_ed25519"
	SSHPollInterval  = 5 * time.Second
	sshConnectTimout = "ConnectTimeout=10"
)

var ErrHostKeyMismatch = errors.New("request: the guest's ssh host key does not match the key it reported (StrictHostKeyChecking=yes)")

var hostKeyMismatch = regexp.MustCompile(`Host key verification failed|REMOTE HOST IDENTIFICATION HAS CHANGED|No .* host key is known`)

var sshRefused = regexp.MustCompile(`Permission denied \(`)

func EnsureKey(ctx context.Context, dir string) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	key := filepath.Join(dir, KeyFile)
	if _, err := os.Stat(key); errors.Is(err, os.ErrNotExist) {
		out, err := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "socialweb-computer-run-request", "-f", key).CombinedOutput()
		if err != nil {
			return "", "", fmt.Errorf("request: ssh-keygen: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(pub)), key, nil
}

type SSH struct {
	Dumbpipe     string
	ProxyCommand string
	Dir          string
}

type Target struct {
	Alias      string
	KnownHosts string
	Ticket     string
}

func (s SSH) Target(c *v1alpha1.ComputeContract) (Target, error) {
	k := c.Status.Contract
	if k == nil || (k.VMAddress == "" && s.ProxyCommand == "") {
		return Target{}, errors.New("request: the contract reports no guest address (onNetwork)")
	}
	if k.HostKeys == nil || len(k.HostKeys.Keys) == 0 {
		return Target{}, errors.New("request: the guest reported no ssh host keys; ssh never runs without them")
	}
	if k.Receipt == nil || k.Receipt.URI == "" {
		return Target{}, errors.New("request: the contract has no receipt")
	}
	ticket := ""
	if s.ProxyCommand == "" {
		var err error
		if ticket, err = Ticket(k.VMAddress); err != nil {
			return Target{}, err
		}
	}
	alias := HostKeyAlias(k.Receipt.URI)
	var b strings.Builder
	for _, h := range k.HostKeys.Keys {
		fmt.Fprintf(&b, "%s %s %s\n", alias, h.Type, h.PublicKey)
	}
	known := filepath.Join(s.Dir, "known_hosts."+alias)
	if err := os.WriteFile(known, []byte(b.String()), 0o600); err != nil {
		return Target{}, err
	}
	return Target{Alias: alias, KnownHosts: known, Ticket: ticket}, nil
}

func (s SSH) args(t Target) []string {
	proxy := s.ProxyCommand
	if proxy == "" {
		proxy = s.Dumbpipe + " connect " + t.Ticket
	}
	return []string{
		"-o", "ProxyCommand=" + proxy,
		"-o", "IdentityFile=" + filepath.Join(s.Dir, KeyFile),
		"-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + t.KnownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "HostKeyAlias=" + t.Alias,
		"-o", "CheckHostIP=no",
		"-o", "LogLevel=ERROR",
	}
}

func (s SSH) WaitReady(ctx context.Context, t Target, timeout time.Duration, progress func(attempt int, err string)) error {
	deadline := time.Now().Add(timeout)
	for attempt := 1; ; attempt++ {
		args := append(s.args(t), "-o", "BatchMode=yes", "-o", sshConnectTimout, "root@"+t.Alias, "true")
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stderr = childproc.FilterSecrets(&stderr)
		err := cmd.Run()
		if err == nil {
			return nil
		}
		msg := strings.TrimSpace(stderr.String())
		if hostKeyMismatch.MatchString(msg) {
			return fmt.Errorf("%w: %s", ErrHostKeyMismatch, msg)
		}
		if sshRefused.MatchString(msg) {
			return fmt.Errorf("request: ssh to %s was refused: %s", t.Alias, msg)
		}
		if progress != nil {
			progress(attempt, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("request: ssh to %s did not answer within %s: %s", t.Alias, timeout, msg)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(SSHPollInterval):
		}
	}
}

func (s SSH) Run(ctx context.Context, t Target, program string, interactive bool, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := s.args(t)
	if interactive {
		args = append(args, "-tt", "root@"+t.Alias)
	} else {
		args = append(args, "root@"+t.Alias, program)
	}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, childproc.FilterSecrets(stderr)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
