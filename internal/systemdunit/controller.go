package systemdunit

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if ok := asExitError(err, &exit); ok {
			return out, &CommandError{Name: name, Args: append([]string(nil), args...), Stderr: string(exit.Stderr), Err: err}
		}
		return out, err
	}
	return out, nil
}

func asExitError(err error, out **exec.ExitError) bool {
	exit, ok := err.(*exec.ExitError)
	if ok {
		*out = exit
	}
	return ok
}

type CommandError struct {
	Name string

	Args []string

	Stderr string

	Err error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("systemdunit: %s %s: %v", e.Name, strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("systemdunit: %s %s: %s", e.Name, strings.Join(e.Args, " "), msg)
}

func (e *CommandError) Unwrap() error { return e.Err }

type Controller struct {
	Runner Runner

	User bool

	Machine string

	Timeout time.Duration

	Binary string
}

func (c Controller) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 30 * time.Second
	}
	return c.Timeout
}

func (c Controller) binary() string {
	if c.Binary == "" {
		return "systemctl"
	}
	return c.Binary
}

func (c Controller) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	runner := c.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	global := []string{}
	if c.User {
		global = append(global, "--user")
	}
	if c.Machine != "" {
		global = append(global, "--machine="+c.Machine)
	}
	return runner.Run(ctx, c.binary(), append(global, args...)...)
}

func (c Controller) DaemonReload(ctx context.Context) error {
	if _, err := c.run(ctx, "daemon-reload"); err != nil {
		return fmt.Errorf("systemdunit: daemon-reload: %w", err)
	}
	return nil
}

func (c Controller) Enable(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.run(ctx, append([]string{"enable"}, names...)...); err != nil {
		return fmt.Errorf("systemdunit: enabling %s: %w", strings.Join(names, " "), err)
	}
	return nil
}

func (c Controller) Disable(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.run(ctx, append([]string{"disable"}, names...)...); err != nil {
		return fmt.Errorf("systemdunit: disabling %s: %w", strings.Join(names, " "), err)
	}
	return nil
}

func (c Controller) Start(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.run(ctx, append([]string{"start"}, names...)...); err != nil {
		return fmt.Errorf("systemdunit: starting %s: %w", strings.Join(names, " "), err)
	}
	return nil
}

func (c Controller) StartNoBlock(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.run(ctx, append([]string{"start", "--no-block"}, names...)...); err != nil {
		return fmt.Errorf("systemdunit: starting %s: %w", strings.Join(names, " "), err)
	}
	return nil
}

func (c Controller) Stop(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.run(ctx, append([]string{"stop"}, names...)...); err != nil {
		return fmt.Errorf("systemdunit: stopping %s: %w", strings.Join(names, " "), err)
	}
	return nil
}

func (c Controller) Restart(ctx context.Context, names ...string) error {
	if len(names) == 0 {
		return nil
	}
	if _, err := c.run(ctx, append([]string{"restart"}, names...)...); err != nil {
		return fmt.Errorf("systemdunit: restarting %s: %w", strings.Join(names, " "), err)
	}
	return nil
}

type Status struct {
	Name string

	ActiveState string

	SubState string

	Result string

	InvocationID string

	MainPID int

	ExecMainCode int

	ExecMainStatus int
}

var statusProperties = []string{"ActiveState", "SubState", "Result", "InvocationID", "MainPID", "ExecMainCode", "ExecMainStatus"}

func (c Controller) Status(ctx context.Context, name string) (Status, error) {
	args := []string{"show", "--no-pager", "-p", strings.Join(statusProperties, ","), name}
	out, err := c.run(ctx, args...)
	if err != nil {
		return Status{}, fmt.Errorf("systemdunit: reading the status of %s: %w", name, err)
	}
	status := Status{Name: name}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "ActiveState":
			status.ActiveState = value
		case "SubState":
			status.SubState = value
		case "Result":
			status.Result = value
		case "InvocationID":
			status.InvocationID = value
		case "MainPID":
			status.MainPID, _ = strconv.Atoi(value)
		case "ExecMainCode":
			status.ExecMainCode, _ = strconv.Atoi(value)
		case "ExecMainStatus":
			status.ExecMainStatus, _ = strconv.Atoi(value)
		}
	}
	return status, nil
}

func (c Controller) IsActive(ctx context.Context, name string) (bool, error) {
	status, err := c.Status(ctx, name)
	if err != nil {
		return false, err
	}
	switch status.ActiveState {
	case "active", "activating", "reloading":
		return true, nil
	}
	return false, nil
}
