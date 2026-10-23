package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap/table"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/sha256sum"
)

var (
	ErrManifestMismatch = errors.New("upgrade: the binary does not match the sha256 it was published with")
	ErrSelfCheck        = errors.New("upgrade: the new binary's self-check failed")
	ErrNoPrevious       = errors.New("rollback: bin/previous names no binary")
	ErrNoCoreUnit       = errors.New("no enabled core unit")
	ErrDevBinary        = errors.New("the binary is a dev build (version says \"dev build, not reproducible\"); install and upgrade take a release build unless --allow-dev-binary is given (dev and test runs only)")
	digestName          = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type UpgradeOptions struct {
	StateDir   string
	Home       string
	RuntimeDir string
	Proc       string
	Binary     string
	SHA256     string
	AllowDev   bool
	Out        io.Writer
	DevStamp   func(ctx context.Context, exe string) (*buildstamp.Info, error)
	SelfCheck  func(ctx context.Context, exe string, role hostcontract.Role, stateDir string) error
	Restart    func(ctx context.Context, units []string) error
}

func (o UpgradeOptions) out() io.Writer {
	if o.Out == nil {
		return io.Discard
	}
	return o.Out
}

func (o UpgradeOptions) bin() string { return filepath.Join(o.StateDir, "bin") }

func isGatewayCore(name string) bool {
	return strings.HasPrefix(name, RuntimePrefix) && strings.HasSuffix(name, "-"+table.GatewayCore)
}

func isBidderCore(name string) bool {
	return strings.HasPrefix(name, RuntimePrefix) && strings.Contains(name, "-"+table.BidderCore) && strings.HasSuffix(name, hostsvc.ServiceSuffix)
}

func InstalledRole(home string) hostcontract.Role {
	entries, _ := os.ReadDir(filepath.Join(home, UserUnitDir))
	for _, e := range entries {
		if isGatewayCore(e.Name()) {
			return hostcontract.RoleGateway
		}
	}
	return hostcontract.RoleBidder
}

func EnabledCoreUnits(home string) []string {
	entries, _ := os.ReadDir(filepath.Join(home, UserUnitDir, WantsDir))
	var out []string
	for _, e := range entries {
		if isGatewayCore(e.Name()) || isBidderCore(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func readTarget(link string) string {
	t, err := os.Readlink(link)
	if err != nil || !digestName.MatchString(t) {
		return ""
	}
	return t
}

func Upgrade(ctx context.Context, o UpgradeOptions) error {
	if !digestName.MatchString(o.SHA256) {
		return fmt.Errorf("upgrade: --sha256 must name the published 64-hex digest of the binary, got %q", o.SHA256)
	}
	sum, err := sha256sum.File(o.Binary)
	if err != nil {
		return err
	}
	if sum != o.SHA256 {
		return fmt.Errorf("%w: %s is %s, published %s", ErrManifestMismatch, o.Binary, sum, o.SHA256)
	}
	if !o.AllowDev && o.DevStamp != nil {
		stamp, err := o.DevStamp(ctx, o.Binary)
		if err != nil {
			return fmt.Errorf("%w: %s version --json: %v", ErrSelfCheck, o.Binary, err)
		}
		if stamp != nil {
			return fmt.Errorf("upgrade: %w: %s (commit %s, built %s)", ErrDevBinary, o.Binary, stamp.Commit, stamp.BuiltUTC)
		}
	}
	current := readTarget(filepath.Join(o.bin(), "current"))
	if current == sum {
		fmt.Fprintf(o.out(), "upgrade: bin/current is already %s; nothing to switch, and a running core is left "+
			"running. If its units are not up, restart them: systemctl --user restart <unit>\n", sum)
		return nil
	}
	dir := filepath.Join(o.bin(), sum)
	dst := filepath.Join(dir, CoreName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if have, _ := sha256sum.File(dst); have != sum {
		if err := copyFile(o.Binary, dst, 0o755); err != nil {
			return err
		}
	}
	if have, err := sha256sum.File(dst); err != nil || have != sum {
		return fmt.Errorf("%w: the copy at %s is %s", ErrManifestMismatch, dst, have)
	}
	role := InstalledRole(o.Home)
	if err := o.SelfCheck(ctx, dst, role, o.StateDir); err != nil {
		return fmt.Errorf("%w: %s: %v; bin/current still names %s", ErrSelfCheck, dst, err, current)
	}
	if current != "" {
		if err := retarget(filepath.Join(o.bin(), "previous"), current); err != nil {
			return err
		}
	}
	if err := retarget(filepath.Join(o.bin(), "current"), sum); err != nil {
		return err
	}
	fmt.Fprintf(o.out(), "upgrade: bin/current -> %s (previous %s)\n", sum, current)
	if err := o.restart(ctx); err != nil {
		return err
	}
	o.collect()
	return nil
}

func (o UpgradeOptions) restart(ctx context.Context) error {
	units := EnabledCoreUnits(o.Home)
	if len(units) == 0 {
		return fmt.Errorf("%w under %s: bin/current is switched but nothing was restarted; restart the core unit by hand",
			ErrNoCoreUnit, filepath.Join(o.Home, UserUnitDir, WantsDir))
	}
	fmt.Fprintf(o.out(), "restarting %s\n", strings.Join(units, " "))
	return o.Restart(ctx, units)
}

func Rollback(ctx context.Context, o UpgradeOptions) error {
	previous := readTarget(filepath.Join(o.bin(), "previous"))
	if previous == "" {
		return ErrNoPrevious
	}
	if _, err := os.Stat(filepath.Join(o.bin(), previous, CoreName)); err != nil {
		return fmt.Errorf("%w: %v", ErrNoPrevious, err)
	}
	current := readTarget(filepath.Join(o.bin(), "current"))
	if err := retarget(filepath.Join(o.bin(), "current"), previous); err != nil {
		return err
	}
	if current != "" {
		if err := retarget(filepath.Join(o.bin(), "previous"), current); err != nil {
			return err
		}
	}
	fmt.Fprintf(o.out(), "rollback: bin/current -> %s (previous %s)\n", previous, current)
	if err := o.restart(ctx); err != nil {
		return err
	}
	o.collect()
	return nil
}

func DevStamp(ctx context.Context, exe string) (*buildstamp.Info, error) {
	b, err := exec.CommandContext(ctx, exe, "version", "--json").Output()
	if err != nil {
		return nil, err
	}
	var v struct {
		Dev *buildstamp.Info `json:"dev"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return v.Dev, nil
}
