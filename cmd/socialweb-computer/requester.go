package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/sshgateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostsvc"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
)

const (
	requesterVerb      = "run requester"
	requesterReadyRead = 10 * time.Second
	requesterRootApply = 3 * time.Minute
)

var requesterOptions = []string{backendOpt, "network", "name", "identity", "state-dir", "ready-file", "fg", "ready-timeout", "network-timeout",
	"kcp-path", "kine-path", "bao-path", "dumbpipe-path", "asset-cache", "log-level", "unit-prefix"}

var requesterPasswordOptions = []string{"name", "state-dir", "network-timeout"}

var requesterHelperOptions = []string{"name", "state-dir", "ready-file", "json"}

var requesterSSHOptions = []string{"name", "state-dir", "ready-file", "contract", "transport", "tag", "exec", "contract-timeout", "network-timeout",
	"dumbpipe-path", "asset-cache", "log-level"}

func parseRequesterSpec(args []string, verb string, verbOptions []string, network bool, other func(compose.AppArg) (bool, error)) (v1alpha1.MarketRequesterSpec, error) {
	root := v1alpha1.MarketRequesterSpec{Profile: marketrequester.RequesterProfileDefault,
		Identity: v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo}}
	parsed, err := compose.ParseAppArgs(marketrequester.RequesterApp, args)
	if err != nil {
		return root, err
	}
	var nv v1alpha1.MarketNetworkValue
	var options []v1alpha1.ApplicationOption
	nvSet, expectHandle := false, false
	for _, p := range parsed {
		switch p.Name {
		case "identity", "pds-state-dir", "expected-pds-url":
			return root, fmt.Errorf("--%s is reserved; pass global flags before --", p.Name)
		}
		perApp := p.To == "spec.secretsFallback" || p.To == "spec.comms.mode" || p.To == "spec.comms.eventSourceRefs" || strings.HasPrefix(p.To, "spec.requesterIngress")
		if network && !perApp && (strings.HasPrefix(p.To, compose.MapToSpec) || strings.HasPrefix(p.To, compose.MapToNetwork)) {
			return root, fmt.Errorf("--%s maps to %s, which the market network sets for its child applications", p.Name, p.To)
		}
		switch {
		case p.To == compose.MapToOptions && !p.Mapped && slices.Contains(verbOptions, p.Name):
			return root, fmt.Errorf("--%s is a %s option; pass it before --", p.Name, verb)
		case p.To == compose.MapToOptions && !p.Mapped:
			return root, fmt.Errorf("--%s is no %s option (%s); %s does not pass unknown options on", p.Name, marketrequester.RequesterApp, compose.OptionMapPath, verb)
		case p.To == compose.MapToOptions:
			options = append(options, v1alpha1.ApplicationOption{Name: p.Name, Value: p.Value})
		case p.To == "spec.identity.mode":
			switch p.Name {
			case "atproto-oauth":
				root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthBrowser
			case "atproto-oauth-qr":
				root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthQr
			default:
				root.Identity.Mode = v1alpha1.MarketIdentityMode(p.Value)
			}
		case p.To == "spec.identity.handle":
			root.Identity.Handle = p.Value
			expectHandle = expectHandle || p.Name == "expect-handle"
		case p.To == "spec.identity.pdsUrl":
			root.Identity.PdsURL = p.Value
		case p.To == "spec.identity.association":
			if p.Value != "false" {
				root.Identity.Association = v1alpha1.AssociationSkip
			}
		case p.To == "spec.bidderDids":
			root.BidderDids = append(root.BidderDids, splitList(p.Value)...)
		case strings.HasPrefix(p.To, "spec.requesterIngress"):
			var applied bool
			if root.RequesterIngress, applied, err = applyIngressOption(root.RequesterIngress, p.Name, p.Value); err != nil {
				return root, err
			}
			if !applied {
				return root, fmt.Errorf("--%s maps to %s, which %s does not take", p.Name, p.To, verb)
			}
		case strings.HasPrefix(p.To, "spec.ingress"):
			var applied bool
			if root.Ingress, applied, err = applyIngressOption(root.Ingress, p.Name, p.Value); err != nil {
				return root, err
			}
			if !applied {
				return root, fmt.Errorf("--%s maps to %s, which %s does not take", p.Name, p.To, verb)
			}
		case p.To == "spec.secretsFallback":
			switch fallback := v1alpha1.SecretsFallback(strings.TrimSpace(p.Value)); fallback {
			case v1alpha1.SecretsFallbackNone, v1alpha1.SecretsFallbackSealed:
				root.SecretsFallback = fallback
			default:
				return root, fmt.Errorf("--secrets-fallback %q: want None or Sealed", p.Value)
			}
		case strings.HasPrefix(p.To, "spec.comms"):
			var applied bool
			if root.Comms, applied, err = applyCommsOption(root.Comms, p.Name, p.Value); err != nil {
				return root, err
			}
			if !applied {
				return root, fmt.Errorf("--%s maps to %s, which %s does not take", p.Name, p.To, verb)
			}
		case strings.HasPrefix(p.To, compose.MapToNetwork):
			nvSet = true
			switch p.To {
			case "network.plcDirectoryUrl":
				nv.PLCDirectoryURL = p.Value
			case "network.ingressProxyHost":
				nv.IngressProxyHost = p.Value
			case "network.qrOrigin":
				nv.QrOrigin = p.Value
			case "network.hosts":
				for _, h := range splitList(p.Value) {
					name, addr, ok := strings.Cut(h, "=")
					if !ok {
						return root, fmt.Errorf("--network-hosts %q: want name=address,...", p.Value)
					}
					nv.Hosts = append(nv.Hosts, v1alpha1.NetworkHost{Name: name, Address: addr})
				}
			}
		default:
			handled := false
			if other != nil {
				if handled, err = other(p); err != nil {
					return root, err
				}
			}
			if !handled {
				return root, fmt.Errorf("--%s maps to %s, which %s does not take", p.Name, p.To, verb)
			}
		}
	}
	if expectHandle {
		switch root.Identity.Mode {
		case v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthDelegated:
		default:
			return root, fmt.Errorf("--expect-handle pins the account of an oauth sign-in; the identity mode %s signs in no existing account, add --atproto-oauth-qr", root.Identity.Mode)
		}
		if _, err := syntax.ParseHandle(strings.TrimSpace(root.Identity.Handle)); err != nil {
			return root, fmt.Errorf("--expect-handle %q: %w", root.Identity.Handle, err)
		}
	}
	if nvSet || !network {
		nv.PLCDirectoryURL = cmp.Or(nv.PLCDirectoryURL, "https://plc.directory")
		nv.IngressProxyHost = cmp.Or(nv.IngressProxyHost, "xrpc.fedproxy.com")
		root.Network = &nv
	}
	if !network && root.Ingress == nil {
		root.Ingress = compose.DefaultIngressConfig()
		if nv.IngressProxyHost != "" {
			root.Ingress.Xrpc.IngressProxyHost = nv.IngressProxyHost
		}
	}
	root.Options = options
	return root, nil
}

type requesterSummary struct {
	request.Ready
	Mode      string  `json:"mode"`
	Network   string  `json:"network,omitempty"`
	Instance  string  `json:"instance"`
	ReadyFile string  `json:"readyFile"`
	Restarted bool    `json:"restarted"`
	Seconds   float64 `json:"seconds"`
}

type requesterRun struct {
	*requestRun
	dir        string
	readyFiles []string
	startedAt  time.Time
	sshKey     string
	mu         sync.Mutex
	last       request.Ready
}

func requesterHome(a *app, vals clicfg.Values, name string) (string, string, error) {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return "", "", err
	}
	if root, err = filepath.Abs(root); err != nil {
		return "", "", err
	}
	if err := request.CheckName(name); err != nil {
		return "", "", err
	}
	return root, request.RequesterDir(root, name), nil
}

func requesterReadyPath(vals clicfg.Values, dir string) (string, error) {
	path := strings.TrimSpace(vals.String("ready-file"))
	if path == "" {
		return filepath.Join(dir, request.ReadyFile), nil
	}
	return filepath.Abs(path)
}

func runRequester(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	started := time.Now()
	pos, pass := splitWords(rest)
	if len(pos) > 0 {
		return a.fail(exitRefused, "run requester: unexpected %q; MarketRequester options go after --", strings.Join(pos, " "))
	}
	backend := hostsvc.BackendProc
	if b := vals.String(backendOpt); b != "" {
		parsed, err := hostsvc.ParseBackend(b)
		if err != nil {
			return a.fail(exitRefused, "run requester: %v", err)
		}
		backend = parsed
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		return a.fail(exitRefused, "run requester: ssh-keygen is not on PATH; install the OpenSSH client (apt install openssh-client, dnf install openssh-clients)")
	}
	name := cmp.Or(strings.TrimSpace(vals.String("name")), "requester")
	stateRoot, dir, err := requesterHome(a, vals, name)
	if err != nil {
		return a.fail(exitRefused, "run requester: %v", err)
	}
	network := strings.TrimSpace(vals.String("network"))
	if network != "" && backend == hostsvc.BackendSystemdUser {
		return a.fail(exitRefused, "run requester: --network places the requester under the market network's devsup; pass --host-backend=proc")
	}
	idName := strings.TrimSpace(vals.String("identity"))
	parsed, err := parseRequesterSpec(pass, requesterVerb, requesterOptions, network != "", nil)
	if err != nil {
		return a.fail(exitRefused, "run requester: %v", err)
	}
	readyFile, err := requesterReadyPath(vals, dir)
	if err != nil {
		return a.fail(exitRefused, "run requester: --ready-file: %v", err)
	}
	networkTimeout, err := time.ParseDuration(vals.String("network-timeout"))
	if err != nil {
		return a.fail(exitRefused, "run requester: network-timeout: %v", err)
	}
	if err := request.EnsureRequesterDir(dir); err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	owner, owned, err := request.TakeOwnerDir(dir)
	if err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	if !owned {
		return a.fail(exitRefused, "run requester: another process runs the requester %s (%s); run requester env|ssh %s reach it", name, dir, name)
	}
	defer owner.Close()
	rec := request.RequesterRecord{Schema: request.RequesterSchema, Name: name, Network: network, Identity: idName, Spec: parsed, CreatedAt: time.Now().UTC()}
	saved, restarted, err := request.ReadRequesterRecord(dir)
	if err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	if restarted {
		switch {
		case len(pass) == 0 && saved.Network == network && saved.Identity == idName:
			rec = saved
		case !saved.SameShape(rec):
			return a.fail(exitRefused, "run requester: the requester %s was created with other options (network %q, identity %q, its own -- options in %s); start it with the same options, or with none after --",
				name, saved.Network, saved.Identity, filepath.Join(dir, request.RequesterFile))
		default:
			rec.CreatedAt = saved.CreatedAt
		}
	} else if err := request.WriteRequesterRecord(dir, rec); err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	mode := request.ModeStandalone
	if network != "" {
		mode = request.ModeNetwork
	}
	rr := &requestRun{a: a, vals: vals, stateRoot: stateRoot, homeDir: dir, verb: requesterVerb, root: rec.Spec, appOptions: rec.Spec.Options,
		networkTimeout: networkTimeout, resuming: true, transport: transportIroh}
	rr.inv = request.Invocation{Name: name, Mode: mode, Network: network, StateRoot: stateRoot, CreatedAt: rec.CreatedAt}
	rr.summary = request.Summary{Name: name, Mode: mode, Network: network, CreatedAt: rec.CreatedAt}
	r := &requesterRun{requestRun: rr, dir: dir, readyFiles: []string{filepath.Join(dir, request.ReadyFile)}, startedAt: time.Now().UTC()}
	if readyFile != r.readyFiles[0] {
		r.readyFiles = append(r.readyFiles, readyFile)
	}
	if err := r.openIdentity(name); err != nil {
		return a.fail(exitRefused, "run requester: %v", err)
	}
	defer func() {
		if r.identity != nil {
			r.identity.Close()
		}
	}()
	pub, key, err := request.EnsureKey(ctx, filepath.Join(dir, request.SSHDir))
	if err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	r.sshKey = key + ".pub"
	if err := r.linkIdentityExposure(); err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	for _, path := range r.readyFiles {
		if err := request.RemoveReady(path); err != nil {
			return a.fail(exitFailed, "run requester: %v", err)
		}
	}
	fresh := true
	if mode == request.ModeNetwork {
		_, netRec, err := r.networkLayout()
		if err != nil {
			return a.fail(exitRefused, "run requester: %v", err)
		}
		_, found, err := request.NetworkApp(ctx, netRec.AdminKubeconfig, name)
		if err != nil {
			return a.fail(exitFailed, "run requester: %v", err)
		}
		fresh = !found
	}
	if backend == hostsvc.BackendSystemdUser {
		r.log("run requester: the installed requester; its unit stops the core and keeps its kcp units and state", "dir", dir, "restart", restarted)
		return r.installedCore(ctx, stateRoot, func(ctx context.Context, rec devinstance.Record, notifyReady func() error) error {
			r.inv.Instance, r.summary.Instance, r.summary.KcpURL = rec.Layout.ID, rec.Layout.ID, rec.KcpURL
			if err := r.attach(ctx, rec.AdminKubeconfig); err != nil {
				return err
			}
			return r.serve(ctx, started, readyFile, pub, restarted, notifyReady)
		})
	}
	r.log("run requester: a long-lived requester; Ctrl-C stops its kcp and keeps its state", "dir", dir, "mode", mode, "network", network, "restart", restarted)
	if err := r.start(ctx, fresh); err != nil {
		r.stop(context.WithoutCancel(ctx), false)
		if ctx.Err() != nil {
			return a.fail(exitFailed, "run requester: stopped before the requester was Ready; its kcp is stopped and its state is kept in %s", dir)
		}
		return a.fail(exitFailed, "run requester: %v", err)
	}
	err = r.serve(ctx, started, readyFile, pub, restarted, func() error { return nil })
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fgDownTimeout)
	defer cancel()
	r.stop(dctx, false)
	if err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	return exitOK
}

func (r *requesterRun) attach(ctx context.Context, kubeconfig string) error {
	k, err := request.OpenKcp(kubeconfig, "")
	if err != nil {
		return err
	}
	r.kubeconfig, r.workspace = kubeconfig, ""
	r.kcp = k.ForContract(r.target)
	if err := applyRequesterRoot(ctx, k, r.root, r.a.log); err != nil {
		return err
	}
	r.summary.KcpRunning = true
	r.log("the requester's kcp runs", "kcp", r.summary.KcpURL, "instance", r.inv.Instance)
	if err := r.signIn(ctx, true); err != nil {
		return fmt.Errorf("signing in: %w", err)
	}
	if err := r.kcp.WaitReady(ctx, r.networkTimeout, func(st v1alpha1.ApplicationStatus) {
		r.log("requester", "phase", string(st.Phase), "message", st.Message, "components", len(st.Components))
	}); err != nil {
		return err
	}
	return request.WriteInvocationIn(r.home(), r.inv)
}

func applyRequesterRoot(ctx context.Context, k *request.Kcp, spec v1alpha1.MarketRequesterSpec, log *slog.Logger) error {
	started := time.Now()
	delay := time.Second
	for attempt := 1; ; attempt++ {
		actx, cancel := context.WithTimeout(ctx, requesterRootApply)
		err := k.ApplyRoot(actx, spec)
		cancel()
		if err == nil {
			return nil
		}
		if time.Since(started) >= requesterRootApply {
			return fmt.Errorf("applying the MarketRequester root object after %d attempts: %w", attempt, err)
		}
		log.Warn("applying the MarketRequester root object; retrying", "attempt", attempt, "err", err.Error())
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		delay = min(2*delay, 15*time.Second)
	}
}

func (r *requesterRun) installedCore(ctx context.Context, stateRoot string, serve func(context.Context, devinstance.Record, func() error) error) int {
	a, vals := r.a, r.vals
	exes := map[string]devinstance.Executable{}
	for _, x := range []struct{ name, option string }{{request.ExeKcp, "kcp-path"}, {request.ExeKine, "kine-path"}, {"bao", "bao-path"},
		{sshgateway.DumbpipeExecutable, "dumbpipe-path"}} {
		e, err := a.optionalExe(ctx, vals, x.name, x.option)
		if err != nil {
			return a.fail(exitRefused, "run requester: %v", err)
		}
		if e.Path != "" {
			exes[x.name] = e
		}
	}
	plugins, err := a.openBaoPluginExes(vals)
	if err != nil {
		return a.fail(exitRefused, "run requester: %v", err)
	}
	maps.Copy(exes, plugins)
	exe, err := selfExe()
	if err != nil {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	exes["socialweb-computer"] = devinstance.Executable{Path: exe}
	readyTimeout, err := time.ParseDuration(vals.String("ready-timeout"))
	if err != nil || readyTimeout < 0 {
		return a.fail(exitRefused, "run requester: ready-timeout %q: want a positive duration", vals.String("ready-timeout"))
	}
	prefix := vals.String("unit-prefix")
	m, err := hostsvc.Open(ctx, hostsvc.Config{Backend: hostsvc.BackendSystemdUser, UnitPrefix: prefix})
	if err != nil {
		return a.fail(exitFailed, "run requester: the systemd-user backend: %v", err)
	}
	defer m.Close()
	err = request.RunCore(ctx, request.CoreConfig{StateDir: stateRoot, Prefix: prefix, Name: r.inv.Name, Executables: exes,
		ReadyTimeout: readyTimeout, Log: a.log, Serve: serve}, m)
	if err != nil && !errors.Is(err, context.Canceled) {
		return a.fail(exitFailed, "run requester: %v", err)
	}
	return exitOK
}

func (r *requesterRun) serve(ctx context.Context, started time.Time, readyFile, pub string, restarted bool, notifyReady func() error) error {
	dir := r.dir
	if err := r.checkIdentity(); err != nil {
		return err
	}
	if err := request.WriteWorkspaceKubeconfig(filepath.Join(dir, request.KubeconfigFile), r.kubeconfig, r.workspace); err != nil {
		return fmt.Errorf("the workspace kubeconfig: %w", err)
	}
	st, err := r.kcp.Root(ctx)
	if err != nil {
		return err
	}
	ready := r.ready(ctx, st)
	if err := r.writeReady(ready); err != nil {
		return fmt.Errorf("the ready file: %w", err)
	}
	r.log("the requester is Ready; the ready file is written", "readyFile", readyFile, "did", ready.Did, "kcp", ready.KcpURL, "workspace", ready.Workspace,
		"sshPublicKey", pub)
	r.a.printJSON(requesterSummary{Ready: ready, Mode: r.inv.Mode, Network: r.inv.Network, Instance: r.inv.Instance, ReadyFile: readyFile, Restarted: restarted,
		Seconds: time.Since(started).Seconds()})
	if err := notifyReady(); err != nil {
		return err
	}
	follow, stopFollow := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := r.kcp.FollowRoot(follow, func(o *v1alpha1.MarketRequester) {
			if err := r.writeReady(r.ready(follow, o.Status)); err != nil {
				r.a.log.Warn("rewriting the ready file", "err", err.Error())
			}
		}); err != nil && follow.Err() == nil {
			r.a.log.Warn("following the MarketRequester for the ready file", "err", err.Error())
		}
	})
	<-ctx.Done()
	stopFollow()
	wg.Wait()
	for _, path := range r.readyFiles {
		if err := request.RemoveReady(path); err != nil {
			r.a.log.Warn("removing the ready file", "path", path, "err", err.Error())
		}
	}
	return nil
}

func runRequesterPassword(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	pos, pass := splitWords(rest)
	if len(pos) != 1 || len(pass) > 0 {
		return a.fail(exitRefused, "run requester password <name>: one requester name; the app password comes on stdin")
	}
	_, dir, err := requesterHome(a, vals, pos[0])
	if err != nil {
		return a.fail(exitRefused, "run requester password: %v", err)
	}
	timeout, err := time.ParseDuration(vals.String("network-timeout"))
	if err != nil {
		return a.fail(exitRefused, "run requester password: network-timeout: %v", err)
	}
	var rec devinstance.Record
	if err := fsx.ReadJSON(filepath.Join(request.InstanceStateDirIn(dir, pos[0]), "instance.json"), &rec); err != nil || rec.AdminKubeconfig == "" {
		return a.fail(exitRefused, "run requester password: the requester %s has no running kcp to hold the password (start it first): %v", pos[0], err)
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		return a.fail(exitFailed, "run requester password: reading stdin: %v", err)
	}
	password := strings.TrimSpace(string(b))
	if password == "" {
		return a.fail(exitRefused, "run requester password: stdin held no password")
	}
	k, err := request.OpenKcp(rec.AdminKubeconfig, rec.BaseWorkspace)
	if err != nil {
		return a.fail(exitFailed, "run requester password: %v", err)
	}
	ref, err := k.PutPdsAccountPassword(ctx, password, timeout)
	if err != nil {
		return a.fail(exitFailed, "run requester password: %v", err)
	}
	a.printJSON(map[string]string{"name": pos[0], "mount": ref.Mount, "path": ref.Path})
	return exitOK
}

func (r *requesterRun) ready(ctx context.Context, st v1alpha1.ApplicationStatus) request.Ready {
	var id *v1alpha1.MarketIdentity
	if ids, err := r.kcp.Identities(); err == nil {
		ictx, cancel := context.WithTimeout(ctx, requesterReadyRead)
		if obj, err := ids.GetMarketIdentity(ictx, marketrequester.RequesterIdentity); err == nil {
			id = obj
		}
		cancel()
	}
	did, handle, mode, pdsURL, ingress := request.ReadyOf(st, id)
	return request.Ready{Schema: request.ReadySchema, Name: r.inv.Name, Pid: os.Getpid(), StartedAt: r.startedAt, KcpURL: r.summary.KcpURL,
		Kubeconfig: filepath.Join(r.dir, request.KubeconfigFile), Workspace: cmp.Or(r.workspace, compose.DefaultWorkspace), Did: did, Handle: handle,
		IdentityMode: mode, PdsURL: pdsURL, Ingress: ingress, SSHPublicKey: r.sshKey}
}

func (r *requesterRun) writeReady(ready request.Ready) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ready.Did == "" {
		ready.Did = r.last.Did
	}
	if r.last.Schema != 0 && r.last.Same(ready) {
		return nil
	}
	for _, path := range r.readyFiles {
		if err := request.WriteReady(path, ready); err != nil {
			return err
		}
	}
	if r.last.Schema != 0 {
		r.log("the ready file is rewritten", "did", ready.Did, "kcp", ready.KcpURL, "pdsIngress", ready.Ingress.Pds, "requesterIngress", ready.Ingress.Requester)
	}
	r.last = ready
	return nil
}

func readRequesterReady(a *app, vals clicfg.Values, rest []string, verb string) (request.Ready, string, string, int) {
	pos, pass := splitWords(rest)
	if len(pos) != 1 || len(pass) > 0 {
		return request.Ready{}, "", "", a.fail(exitRefused, "run requester %s <name>: one requester name", verb)
	}
	stateRoot, dir, err := requesterHome(a, vals, pos[0])
	if err != nil {
		return request.Ready{}, "", "", a.fail(exitRefused, "run requester %s: %v", verb, err)
	}
	path, err := requesterReadyPath(vals, dir)
	if err != nil {
		return request.Ready{}, "", "", a.fail(exitRefused, "run requester %s: --ready-file: %v", verb, err)
	}
	ready, err := request.ReadReady(path)
	if err != nil {
		return ready, "", "", a.fail(exitRefused, "run requester %s: %v", verb, err)
	}
	if ready.Name != pos[0] {
		return ready, "", "", a.fail(exitRefused, "run requester %s: the ready file %s is the requester %s's, not %s", verb, path, ready.Name, pos[0])
	}
	return ready, stateRoot, dir, exitOK
}

func runRequesterEnv(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	ready, _, dir, code := readRequesterReady(a, vals, rest, "env")
	if code != exitOK {
		return code
	}
	ex := []devinstance.Export{
		{Name: "KUBECONFIG", Value: ready.Kubeconfig},
		{Name: "SOCIALWEB_KCP_HOST", Value: ready.KcpURL},
		{Name: "SOCIALWEB_KCP_WORKSPACE", Value: ready.Workspace},
		{Name: "SOCIALWEB_REQUESTER_NAME", Value: ready.Name},
		{Name: "SOCIALWEB_REQUESTER_DID", Value: ready.Did},
		{Name: "SOCIALWEB_REQUESTER_DIR", Value: dir},
		{Name: "SOCIALWEB_REQUESTER_SSH_PUBLIC_KEY", Value: ready.SSHPublicKey},
	}
	if vals.Bool("json") {
		a.printJSON(exportMap(ex))
		return exitOK
	}
	fmt.Fprint(a.stdout, devinstance.ShellExports(ex))
	return exitOK
}

func (a *app) openRequester(ctx context.Context, vals clicfg.Values, rest []string, verb string) (*requestRun, int) {
	ready, stateRoot, dir, code := readRequesterReady(a, vals, rest, verb)
	if code != exitOK {
		return nil, code
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return nil, a.fail(exitRefused, "run requester %s: %v", verb, err)
	}
	r.stateRoot, r.homeDir, r.verb = stateRoot, dir, requesterVerb
	r.inv = request.Invocation{Name: ready.Name, StateRoot: stateRoot}
	k, err := request.OpenKcp(ready.Kubeconfig, ready.Workspace)
	if err != nil {
		return nil, a.fail(exitFailed, "run requester %s: %v", verb, err)
	}
	actx, cancel := context.WithTimeout(ctx, requestAttachProbe)
	defer cancel()
	st, err := k.Root(actx)
	if err != nil {
		return nil, a.fail(exitFailed, "run requester %s: the requester's kcp does not answer (is run requester %s running?): %v", verb, ready.Name, err)
	}
	if st.Phase != v1alpha1.ApplicationPhaseReady {
		return nil, a.fail(exitFailed, "run requester %s: the MarketRequester is %s: %s", verb, st.Phase, st.Message)
	}
	if r.target == "" {
		return nil, a.fail(exitRefused, "run requester %s: --contract names the ComputeContract (a run requester has no contract of its own)", verb)
	}
	r.kcp = k.ForContract(r.target)
	return r, exitOK
}

func runRequesterSSH(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.openRequester(ctx, vals, rest, "ssh")
	if r == nil {
		return code
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return a.fail(exitRefused, "run requester ssh: ssh is not on PATH; install the OpenSSH client (apt install openssh-client, dnf install openssh-clients)")
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		return a.fail(exitFailed, "run requester ssh: %v", err)
	}
	if c.Status.Phase != v1alpha1.ComputeContractHeld {
		return a.fail(exitFailed, "run requester ssh: the contract is %s, not Held: %s", c.Status.Phase, c.Status.Message)
	}
	ec, err := r.ssh(ctx, c)
	if err != nil {
		return a.fail(exitFailed, "run requester ssh: %v", err)
	}
	if ec != 0 {
		return exitFailed
	}
	return exitOK
}

func runRequesterProxy(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	return proxyRequest(ctx, a, vals, requesterVerb+" proxy", func(q *app) (*requestRun, bool, int) {
		r, code := q.openRequester(ctx, vals, rest, "proxy")
		return r, false, code
	})
}
