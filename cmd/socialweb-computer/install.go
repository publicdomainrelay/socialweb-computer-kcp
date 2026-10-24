package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/buildstamp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/install"
)

var installOptions = []string{backendOpt, "handle", "account", "state-dir", "vm-slots", "dry-run", "adopt-account", "json", "root", "unit-prefix", "allow-dev-binary",
	install.OptKcpFront, install.OptTLSFront, install.OptCertFront, install.OptSpecFile, install.OptGuestImg, install.OptGuestImgBase, install.OptReadyTimeout, install.OptNetworkTimeout, install.OptNoGuestLANUDP, install.OptGuestHostUDP}

var uninstallOptions = []string{"account", "purge", "force", "json", "root"}

func splitHandles(s string) []string {
	var out []string
	for h := range strings.SplitSeq(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func installExit(a *app, verb string, rep install.Report, err error, asJSON bool) int {
	if asJSON {
		a.printJSON(rep)
	} else {
		printSteps(a, rep)
	}
	if err == nil {
		return exitOK
	}
	fmt.Fprintf(a.stderr, "%s: %v\n", verb, err)
	switch {
	case errors.Is(err, install.ErrProcBackend), errors.Is(err, install.ErrForeignAccount), errors.Is(err, install.ErrVMsRunning),
		errors.Is(err, install.ErrManagerWithoutKVM), errors.Is(err, install.ErrNoRecord), errors.Is(err, install.ErrUnsupported),
		errors.Is(err, install.ErrDevBinary), errors.Is(err, install.ErrBinaryMismatch), errors.Is(err, install.ErrUsage):
		return exitRefused
	}
	return exitFailed
}

func printSteps(a *app, rep install.Report) {
	if len(rep.Steps) == 0 {
		return
	}
	for _, s := range rep.Steps {
		fmt.Fprintf(a.stdout, "%-15s %-20s %s\n", strings.ToUpper(s.Outcome), s.Name, s.Detail)
	}
}

func runInstall(role hostcontract.Role) verbFunc {
	verb := "install " + string(role)
	return func(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
		if len(rest) > 0 {
			return a.fail(exitRefused, "%s: unexpected arguments %q", verb, rest)
		}
		if vals.String(backendOpt) == "proc" {
			return installExit(a, verb, install.Report{}, install.ErrProcBackend, false)
		}
		dry := vals.Bool("dry-run")
		root := vals.String("root")
		staged := install.Rooted(root)
		if os.Geteuid() != 0 && !dry && !staged {
			return a.fail(exitRefused, "%s: the one explicit root step: run it as root (or --dry-run to print the step list, or --root for a staged host)", verb)
		}
		if staged && os.Geteuid() == 0 {
			return a.fail(exitRefused, "%s: --root stages a host for the invoking user's own manager; run it as that user, never as root", verb)
		}
		if stamp := buildstamp.Read(); stamp != nil && !vals.Bool("allow-dev-binary") {
			return installExit(a, verb, install.Report{}, fmt.Errorf("%w: this binary (commit %s, built %s)", install.ErrDevBinary, stamp.Commit, stamp.BuiltUTC), false)
		}
		exe, err := foldmain.ResolveExe()
		if err != nil {
			return a.fail(exitFailed, "%s: resolving this binary: %v", verb, err)
		}
		sys, err := a.installSystem(ctx, root)
		if err != nil {
			return installExit(a, verb, install.Report{}, err, false)
		}
		listen := map[string]string{}
		passed := func(k string) string {
			if s := vals.Source(k); s == clicfg.SourceFlag || s == clicfg.SourceEnv {
				return vals.String(k)
			}
			return ""
		}
		for _, k := range install.Passed {
			if v := passed(k); v != "" {
				listen[k] = v
			}
		}
		rep, err := install.Install(ctx, install.Options{
			Role: role, Handles: splitHandles(vals.String("handle")), Account: installAccount(vals, staged),
			StateDir: vals.String("state-dir"), Binary: exe, VMSlots: vals.Int("vm-slots"), DryRun: dry,
			AdoptAccount: vals.Bool("adopt-account"), HostBackend: vals.String(backendOpt), Out: a.stdout,
			Root: root, Staged: staged, UnitPrefix: vals.String("unit-prefix"), Settings: listen,
			ReadyTimeout: passed(install.OptReadyTimeout), NetworkTimeout: passed(install.OptNetworkTimeout),
			GuestImageBase: vals.String(install.OptGuestImgBase), GuestImageBaseResolved: true,
		}, sys)
		return installExit(a, verb, rep, err, vals.Bool("json"))
	}
}

func (a *app) installSystem(ctx context.Context, root string) (install.System, error) {
	if install.Rooted(root) {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("--root %q is not absolute", root)
		}
		return install.NewStagedSystem(root, a.stdout)
	}
	return install.NewSystem(ctx, a.stdout)
}

func runUninstall(role hostcontract.Role) verbFunc {
	verb := "uninstall " + string(role)
	return func(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
		if len(rest) > 0 {
			return a.fail(exitRefused, "%s: unexpected arguments %q", verb, rest)
		}
		root := vals.String("root")
		staged := install.Rooted(root)
		if os.Geteuid() != 0 && !staged {
			return a.fail(exitRefused, "%s: run it as root", verb)
		}
		sys, err := a.installSystem(ctx, root)
		if err != nil {
			return installExit(a, verb, install.Report{}, err, false)
		}
		rep, err := install.Uninstall(ctx, install.UninstallOptions{Role: role, Account: installAccount(vals, staged),
			Purge: vals.Bool("purge"), Force: vals.Bool("force"), Root: root, Staged: staged, Out: a.stdout}, sys)
		return installExit(a, verb, rep, err, vals.Bool("json"))
	}
}

var upgradeOptions = []string{"binary", "sha256", "state-dir", "allow-dev-binary", "root"}

func installedBinaries(state string) []string {
	return []string{filepath.Join(state, "bin", "current", install.CoreName), filepath.Join(state, "bin", "previous", install.CoreName)}
}

func (a *app) upgradeOptions(ctx context.Context, verb string, vals clicfg.Values) (install.UpgradeOptions, error) {
	if os.Geteuid() == 0 {
		return install.UpgradeOptions{}, fmt.Errorf("%s runs as the installed account, never as root", verb)
	}
	state := vals.String("state-dir")
	if state == "" {
		return install.UpgradeOptions{}, fmt.Errorf("%s: --state-dir (SOCIALWEB_STATE_DIR) names the account's state dir; it is read from that flag or env var, never from a config.json", verb)
	}
	if !filepath.IsAbs(state) {
		return install.UpgradeOptions{}, fmt.Errorf("%s: state-dir %q is not absolute", verb, state)
	}
	u, err := user.Current()
	if err != nil {
		return install.UpgradeOptions{}, fmt.Errorf("%s: the invoking account: %w", verb, err)
	}
	home := u.HomeDir
	if root := vals.String("root"); install.Rooted(root) {
		sys, err := a.installSystem(ctx, root)
		if err != nil {
			return install.UpgradeOptions{}, err
		}
		acct, ok, err := sys.LookupAccount(u.Username)
		if err != nil {
			return install.UpgradeOptions{}, err
		}
		if !ok {
			return install.UpgradeOptions{}, fmt.Errorf("%s: %s is no account of the staged host %s", verb, u.Username, root)
		}
		home = acct.Home
	}
	return install.UpgradeOptions{StateDir: state, Home: home, RuntimeDir: install.OwnRuntimeDir(), Out: a.stdout,
		SelfCheck: install.SelfCheck, Restart: install.RestartOwnUnits}, nil
}

func runUpgrade(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	o, err := a.upgradeOptions(ctx, "upgrade", vals)
	if err != nil {
		return a.fail(exitRefused, "%v", err)
	}
	o.Binary, o.SHA256, o.AllowDev, o.DevStamp = vals.String("binary"), vals.String("sha256"), vals.Bool("allow-dev-binary"), install.DevStamp
	if o.Binary == "" {
		return a.fail(exitRefused, "upgrade: --binary names the new socialweb-computer executable")
	}
	if err := install.Upgrade(ctx, o); err != nil {
		code := exitFailed
		if errors.Is(err, install.ErrManifestMismatch) || errors.Is(err, install.ErrSelfCheck) || errors.Is(err, install.ErrDevBinary) {
			code = exitRefused
		}
		return a.fail(code, "%v", err)
	}
	return exitOK
}

func runRollback(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	o, err := a.upgradeOptions(ctx, "rollback", vals)
	if err != nil {
		return a.fail(exitRefused, "%v", err)
	}
	if err := install.Rollback(ctx, o); err != nil {
		code := exitFailed
		if errors.Is(err, install.ErrNoPrevious) {
			code = exitRefused
		}
		return a.fail(code, "%v", err)
	}
	return exitOK
}

func installAccount(vals clicfg.Values, staged bool) string {
	if a := vals.String("account"); a != "" || !staged {
		return a
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
