package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/assets"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/capabilities"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/ccmmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/checkhostmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewaymain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/providermain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tenantcredentialmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vaultmain"
)

const (
	exitOK      = 0
	exitFailed  = 1
	exitRefused = 2
	serviceName = "socialweb-computer"
	backendOpt  = "host-backend"
	appRoleOpt  = "app-role"
)

type app struct {
	env    clicfg.Env
	stdout io.Writer
	stderr io.Writer
	spec   clicfg.Spec
	config map[string]any
	goos   string
	goarch string
	log    *slog.Logger
}

type verbFunc func(ctx context.Context, a *app, v clicfg.Values, rest []string) int

type builtVerb struct {
	options []string
	run     verbFunc
}

func builtVerbs() map[string]builtVerb {
	verbs := map[string]builtVerb{
		"version":                {[]string{"json"}, runVersion},
		"dev up":                 {slices.Concat([]string{backendOpt, "instance", "state-dir", "profile", "bidders", "with-gateway", "app-values", "network-timeout", "stand-ins", "parent", "die-with", "kcp-path", "kine-path", "postgres-root", "bao-path", "openbao-plugin-dir", "dumbpipe-path", "openbao-addr", "openbao-token-file", "asset-cache", "ready-timeout", "kcp-base-workspace", "fg", "keep-state", "guest-image-release-base", "no-guest-lan-udp", "guest-host-udp"}, devVMOptions), runDevUp},
		"dev host":               {[]string{backendOpt, "instance", "state-dir", "profile", "die-with", "kcp-path", "kine-path", "postgres-root", "bao-path", "asset-cache", "ready-timeout", "kcp-base-workspace", "fg", "keep-state"}, runDevHost},
		"dev status":             {[]string{"instance", "state-dir"}, runDevStatus},
		"status":                 {[]string{"instance", "state-dir"}, runStatus},
		"dev down":               {[]string{"instance", "state-dir", "keep-state"}, runDevDown},
		"dev ls":                 {[]string{"state-dir"}, runDevLs},
		"dev logs":               {[]string{"instance", "state-dir", "tail"}, runDevLogs},
		"dev env":                {[]string{"instance", "state-dir", "json"}, runDevEnv},
		"dev check":              {nil, runDevCheck},
		"dev restart":            {[]string{"instance", "state-dir", "binary", "ready-timeout"}, runDevRestart},
		"dev links":              {[]string{"instance", "state-dir", "json"}, runDevLinks},
		"dev gc":                 {[]string{"asset-cache", "state-dir", "asset-gc-window"}, runDevGC},
		"apply":                  {applyOptions, runApply},
		"run bidder":             {bidderOptions, runBidder},
		"run request":            {requestOptions, runRequest},
		"run gateway ssh":        {gatewaySshOptions, runGatewaySsh},
		"run request list":       {[]string{"state-dir", "json"}, runRequestList},
		"run request release":    {requestOptions, runRequestRelease},
		"run request resume":     {requestOptions, runRequestResume},
		"run request ssh":        {append(slices.Clone(requestOptions), "contract"), runRequestSSH},
		"run request proxy":      {append(slices.Clone(requestOptions), "log-level", "contract"), runRequestProxy},
		"manifests list":         {[]string{"json"}, runManifestsList},
		"manifests render":       {renderOptions, runManifestsRender},
		"install gateway":        {installOptions, runInstall(hostcontract.RoleGateway)},
		"install bidder":         {installOptions, runInstall(hostcontract.RoleBidder)},
		"uninstall gateway":      {uninstallOptions, runUninstall(hostcontract.RoleGateway)},
		"uninstall bidder":       {uninstallOptions, runUninstall(hostcontract.RoleBidder)},
		"upgrade":                {upgradeOptions, runUpgrade},
		"rollback":               {[]string{"state-dir", "root"}, runRollback},
		"units list":             {[]string{backendOpt, "instance", "state-dir", "asset-cache", "json"}, runUnitsList},
		"units audit":            {[]string{backendOpt, "instance", "state-dir", "asset-cache", "json"}, runUnitsAudit},
		"units roll kcp":         {[]string{backendOpt, "state-dir", "handle", "kcp-path", "kine-path", "planned-outage", "ready-timeout", "unit-prefix", "store-backup-keep", "store-backup-keep-days"}, runUnitsRollKcp},
		"units roll cluster":     {[]string{"cluster-state-dir", "k3s-path", "kine-path", "idle-bound", "ready-timeout"}, runUnitsRoll("cluster")},
		"units roll link":        {[]string{"cluster-state-dir", "service", "dumbpipe-path", "ready-timeout"}, runUnitsRoll("link")},
		"internal ns-exec":       {[]string{"private-hostname", "bind", "inside"}, runNsExec},
		"internal iroh-dial":     {[]string{"socket", "dumbpipe-path"}, runIrohDial},
		"internal ingress-relay": {ingressRelayOptions, runIngressRelay},
		"internal atproto-relay": {atprotoRelayOptions, runAtprotoRelay},
		"internal devsup":        {[]string{"state", "socket", "die-with", "instance"}, runDevsup},
		"internal dev-core":      {[]string{"instance-dir", "ready-timeout"}, runDevCore},
		"internal hostprocesses": {[]string{backendOpt, "instance", "state-dir", "kcp-kubeconfig", "kcp-base-workspace", "bao-path"}, runHostProcesses},
		"dev roll kcp":           {[]string{"instance", "state-dir", "ready-timeout", "store-backup-keep", "store-backup-keep-days"}, runDevRoll},
		"internal vm start":      {vmOptions, runVM("start")},
		"internal vm inspect":    {vmOptions, runVM("inspect")},
		"internal vm stop":       {vmOptions, runVM("stop")},
		"internal vm list":       {vmOptions, runVM("list")},
		"serve gateway kcp":      {serveGatewayOptions, runServeGatewayKcp},
		"internal apiserver-front": {[]string{"owner", "kcp-front-address", "front-control", "front-state", "front-fd-store"},
			runApiserverFront},
	}
	maps.Copy(verbs, releaseBuiltVerbs())
	return verbs
}

type foldedVerb struct {
	prefix []string
	main   func(context.Context, foldmain.Invocation) int
}

func foldedVerbs() map[string]foldedVerb {
	verbs := map[string]foldedVerb{
		"serve gateway xrpc":                {nil, gatewaymain.Main},
		"serve provider":                    {nil, providermain.Main},
		"migrate":                           {[]string{"migrate"}, providermain.Main},
		"vault apply-root":                  {[]string{"apply-root"}, vaultmain.Main},
		"check-host":                        {nil, checkhostmain.Main},
		"internal cloud-controller-manager": {nil, ccmmain.Main},
		"internal tenantcredential":         {nil, tenantcredentialmain.Main},
	}
	maps.Copy(verbs, devFoldedVerbs())
	return verbs
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], clicfg.OSEnv{}, os.Stdout, os.Stderr, runtime.GOOS, runtime.GOARCH)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, env clicfg.Env, stdout, stderr io.Writer, goos, goarch string) int {
	spec, err := clicfg.Load()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitFailed
	}
	config, err := clicfg.ConfigFile(env)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRefused
	}
	a := &app{env: env, stdout: stdout, stderr: stderr, spec: spec, config: config, goos: goos, goarch: goarch,
		log: slog.New(slog.NewJSONHandler(stderr, nil)).With("service", serviceName)}
	if err := assets.Ready(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", serviceName, err)
		return exitRefused
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		a.usage(stdout, "")
		if len(args) == 0 {
			return exitRefused
		}
		return exitOK
	}
	verb, rest, ok := capabilities.Find(args)
	if !ok {
		group := strings.Join(args, " ")
		if capabilities.Groups()[args[0]] {
			a.usage(stderr, args[0])
			return exitRefused
		}
		fmt.Fprintf(stderr, "%s: unknown subcommand %q\n", serviceName, group)
		a.usage(stderr, "")
		return exitRefused
	}
	if why, missing := capabilities.Missing(verb, goos, goarch); missing {
		fmt.Fprintln(stderr, why)
		return exitRefused
	}
	if f, ok := foldedVerbs()[verb.Path]; ok {
		return a.runFolded(ctx, verb, f, rest)
	}
	built, isBuilt := builtVerbs()[verb.Path]
	names := built.options
	if !isBuilt {
		names = nil
		for _, n := range verb.Needs {
			if n == capabilities.NeedHostBackend {
				names = append(names, backendOpt)
			}
		}
	}
	vals, rest, err := spec.Resolve(verb.Path, names, flagsFirst(spec, rest), env, config, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return exitRefused
	}
	for _, n := range verb.Needs {
		if n != capabilities.NeedHostBackend {
			continue
		}
		if err := spec.Require(vals, backendOpt); err != nil {
			fmt.Fprintf(stderr, "%s: %v: this verb places host processes and never probes for a backend (systemd-user | proc)\n", verb.Path, err)
			return exitRefused
		}
		if _, err := hostsvc.ParseBackend(vals.String(backendOpt)); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", verb.Path, err)
			return exitRefused
		}
	}
	if !isBuilt {
		fmt.Fprintf(stderr, "%s: not built yet; built by %s\n", verb.Path, verb.BuiltBy)
		return exitRefused
	}
	return built.run(ctx, a, vals, rest)
}

func (a *app) runFolded(ctx context.Context, verb capabilities.Verb, f foldedVerb, rest []string) int {
	exe, err := foldmain.ResolveExe()
	if err != nil {
		fmt.Fprintf(a.stderr, "%s: resolving this binary's own path: %v\n", verb.Path, err)
		return exitFailed
	}
	return f.main(ctx, foldmain.Invocation{
		Args: append(append([]string(nil), f.prefix...), rest...),
		Getenv: func(k string) string {
			v, _ := a.env.LookupEnv(k)
			return v
		},
		Stdout: a.stdout, Stderr: a.stderr, Exe: exe, Verb: verb.Words(),
	})
}

func flagsFirst(spec clicfg.Spec, args []string) []string {
	var flags, words []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			words = append(words, args[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			words = append(words, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if o, ok := spec.Options[name]; ok && o.Type != clicfg.TypeBoolean && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, words...)
}

func (a *app) usage(w io.Writer, group string) {
	fmt.Fprintf(w, "usage: %s <subcommand> [options]   (%s/%s)\n\n", serviceName, a.goos, a.goarch)
	verbs := capabilities.Tree()
	sort.SliceStable(verbs, func(i, j int) bool { return verbs[i].Path < verbs[j].Path })
	for _, v := range verbs {
		if group != "" && !strings.HasPrefix(v.Path, group+" ") {
			continue
		}
		var marks []string
		if why, missing := capabilities.Missing(v, a.goos, a.goarch); missing {
			marks = append(marks, "no backend here: "+why[len(v.Path)+2:])
		}
		if !v.Built() {
			marks = append(marks, "not built: "+v.BuiltBy)
		}
		mark := ""
		if len(marks) > 0 {
			mark = "  [" + strings.Join(marks, "; ") + "]"
		}
		fmt.Fprintf(w, "  %-34s %s%s\n", v.Path, v.Summary, mark)
	}
	fmt.Fprintf(w, "\nconfig: flag > env > %s file > default (internal/clicfg/options.json)\n", clicfg.ConfigPathEnv)
}
