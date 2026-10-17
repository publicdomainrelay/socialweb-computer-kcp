package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appcli"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bootstrap"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/request"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tsbundle"
)

const (
	requestSSHReady     = time.Minute
	requestIdentityWait = 5 * time.Minute
)

var requestOptions = append([]string{"network", "name", "secrets", "hold", "exec", "skip-ssh", "contract-timeout", "state-dir",
	"ready-timeout", "network-timeout", "kcp-path", "kine-path", "deno-path", "bao-path", "dumbpipe-path", "asset-cache"},
	devOnly(tsbundle.SourceRootOption)...)

type requestRun struct {
	a               *app
	vals            clicfg.Values
	stateRoot       string
	inv             request.Invocation
	hold            bool
	skipSSH         bool
	exec            string
	secrets         string
	root            v1alpha1.MarketRequesterSpec
	contract        v1alpha1.ComputeContractSpec
	appOptions      []v1alpha1.ApplicationOption
	contractTimeout time.Duration
	networkTimeout  time.Duration
	summary         request.Summary
	kcp             *request.Kcp
	standalone      devinstance.Layout
	instanceDir     string
}

func splitWords(rest []string) ([]string, []string) {
	i := 0
	for i < len(rest) && !strings.HasPrefix(rest[i], "--") {
		i++
	}
	pass := rest[i:]
	if len(pass) > 0 && pass[0] == "--" {
		pass = pass[1:]
	}
	return rest[:i], pass
}

func (r *requestRun) log(msg string, args ...any) {
	r.a.log.Info(msg, append([]any{"request", r.inv.Name, "utc", time.Now().UTC().Format(time.RFC3339)}, args...)...)
}

func newRequestRun(a *app, vals clicfg.Values) (*requestRun, error) {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return nil, err
	}
	r := &requestRun{a: a, vals: vals, stateRoot: root, hold: vals.Bool("hold"), skipSSH: vals.Bool("skip-ssh"),
		exec: vals.String("exec"), secrets: vals.String("secrets")}
	if r.contractTimeout, err = time.ParseDuration(vals.String("contract-timeout")); err != nil {
		return nil, fmt.Errorf("contract-timeout: %w", err)
	}
	if r.networkTimeout, err = time.ParseDuration(vals.String("network-timeout")); err != nil {
		return nil, fmt.Errorf("network-timeout: %w", err)
	}
	return r, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (r *requestRun) passthrough(args []string, network bool) error {
	parsed, err := appctl.ParseAppArgs(appctl.RequesterApp, args)
	if err != nil {
		return err
	}
	r.root = v1alpha1.MarketRequesterSpec{Profile: appctl.RequesterProfileDefault,
		Identity: v1alpha1.ApplicationIdentity{Mode: v1alpha1.MarketIdentityModeLocalRepo}}
	var nv v1alpha1.MarketNetworkValue
	nvSet := false
	for _, p := range parsed {
		if network && (strings.HasPrefix(p.To, appctl.MapToSpec) || strings.HasPrefix(p.To, appctl.MapToNetwork)) {
			return fmt.Errorf("--%s maps to %s, which the market network sets for its child applications", p.Name, p.To)
		}
		switch {
		case p.To == appctl.MapToOptions:
			r.appOptions = append(r.appOptions, v1alpha1.ApplicationOption{Name: p.Name, Value: p.Value})
		case p.To == appctl.MapToCLI:
			switch p.Name {
			case "exec":
				r.exec = p.Value
			case "hold", "keep-vm":
				r.hold = p.Value != "false"
			case "skip-ssh":
				r.skipSSH = p.Value != "false"
			}
		case p.To == "spec.identity.mode":
			switch p.Name {
			case "atproto-oauth":
				r.root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthBrowser
			case "atproto-oauth-qr":
				r.root.Identity.Mode = v1alpha1.MarketIdentityModeOAuthQr
			default:
				r.root.Identity.Mode = v1alpha1.MarketIdentityMode(p.Value)
			}
		case p.To == "spec.identity.handle":
			r.root.Identity.Handle = p.Value
		case p.To == "spec.identity.association":
			if p.Value != "false" {
				r.root.Identity.Association = v1alpha1.AssociationSkip
			}
		case p.To == "spec.bidderDids":
			r.root.BidderDids = append(r.root.BidderDids, splitList(p.Value)...)
		case strings.HasPrefix(p.To, appctl.MapToNetwork):
			nvSet = true
			switch p.To {
			case "network.plcDirectoryUrl":
				nv.PlcDirectoryUrl = p.Value
			case "network.ingressProxyHost":
				nv.IngressProxyHost = p.Value
			case "network.relayUrls":
				nv.RelayUrls = splitList(p.Value)
			case "network.firehoseUrls":
				nv.FirehoseUrls = splitList(p.Value)
			case "network.qrOrigin":
				nv.QrOrigin = p.Value
			case "network.hosts":
				for _, h := range splitList(p.Value) {
					name, addr, ok := strings.Cut(h, "=")
					if !ok {
						return fmt.Errorf("--network-hosts %q: want name=address,...", p.Value)
					}
					nv.Hosts = append(nv.Hosts, v1alpha1.NetworkHost{Name: name, Address: addr})
				}
			}
		case p.To == "contract.denyBidderDids":
			r.contract.DenyBidderDids = append(r.contract.DenyBidderDids, splitList(p.Value)...)
		case p.To == "contract.vmName":
			r.contract.VMName = p.Value
		case p.To == "contract.userData":
			b, err := os.ReadFile(p.Value)
			if err != nil {
				return fmt.Errorf("--user-data: %w", err)
			}
			r.contract.UserData = string(b)
		case p.To == "contract.sshAuthorizedKeys":
			r.contract.SshAuthorizedKeys = append(r.contract.SshAuthorizedKeys, splitList(p.Value)...)
		case p.To == "contract.policy":
			r.contract.Policy = p.Value
		case p.To == "contract.policyArgs":
			r.contract.PolicyArgs = p.Value
		case p.To == "contract.vmReadyTimeoutSec":
			n, err := strconv.Atoi(p.Value)
			if err != nil {
				return fmt.Errorf("--vm-ready-timeout-sec %q: %w", p.Value, err)
			}
			r.contract.VMReadyTimeoutSec = n
		case p.To == "contract.secretsRef":
			r.secrets = p.Value
		default:
			return fmt.Errorf("--%s maps to %s, which run request does not take", p.Name, p.To)
		}
	}
	if nvSet {
		if nv.PlcDirectoryUrl == "" {
			nv.PlcDirectoryUrl = "https://plc.directory"
		}
		if nv.IngressProxyHost == "" {
			nv.IngressProxyHost = "xrpc.fedproxy.com"
		}
		if nv.RelayUrls == nil {
			nv.RelayUrls = []string{}
		}
		if nv.FirehoseUrls == nil {
			nv.FirehoseUrls = []string{}
		}
		r.root.Network = &nv
	}
	r.root.Options = r.appOptions
	return nil
}

func readSecrets(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Path  string `json:"path"`
		Value any    `json:"value"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s is not a JSON array of {path, value}: %w", path, err)
	}
	for i, e := range entries {
		if !strings.HasPrefix(e.Path, "/") || strings.Contains(e.Path, "/../") {
			return nil, fmt.Errorf("%s: entry %d path %q is not an absolute path", path, i, e.Path)
		}
	}
	return b, nil
}

func (r *requestRun) networkLayout() (devinstance.Layout, devinstance.Record, error) {
	l, err := r.a.layoutOf(r.inv.Network, r.vals)
	if err != nil {
		return l, devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := devinstance.ReadJSON(l.Record, &rec); err != nil {
		return l, rec, fmt.Errorf("market network %s has no instance record (dev up --profile market-network first): %w", r.inv.Network, err)
	}
	if rec.Profile != bootstrap.ProfileMarketNetwork || rec.AdminKubeconfig == "" {
		return l, rec, fmt.Errorf("instance %s is not a market network (profile %s)", r.inv.Network, rec.Profile)
	}
	return l, rec, nil
}

func (r *requestRun) childRecord(netLayout devinstance.Layout, instance string) (devinstance.Record, error) {
	cl, err := devinstance.NewLayout(instance, filepath.Join(netLayout.StateDir, appctl.AppsDir), filepath.Join(netLayout.RuntimeDir, appctl.AppsDir))
	if err != nil {
		return devinstance.Record{}, err
	}
	var rec devinstance.Record
	if err := devinstance.ReadJSON(cl.Record, &rec); err != nil {
		return rec, err
	}
	return rec, nil
}

func (r *requestRun) start(ctx context.Context, fresh bool) error {
	started := time.Now()
	var kubeconfig, workspace string
	switch r.inv.Mode {
	case request.ModeNetwork:
		netLayout, netRec, err := r.networkLayout()
		if err != nil {
			return err
		}
		if fresh {
			app := v1alpha1.NetworkApp{Name: r.inv.Name, Kind: v1alpha1.ApplicationKindMarketRequester,
				Profile: appctl.RequesterProfileNetwork, Options: r.appOptions}
			if err := request.AddToNetwork(ctx, netRec.AdminKubeconfig, app); err != nil {
				return err
			}
		} else if err := request.SetStopped(ctx, netRec.AdminKubeconfig, r.inv.Name, false); err != nil {
			return err
		}
		r.log("the market network places the invocation in its workspace of the network's kcp", "network", r.inv.Network)
		app, err := request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
			return a.Instance != "" && a.KcpUrl != "" && a.Message != appctl.StoppedMessage && a.Phase != v1alpha1.ApplicationPhasePending
		}, func(a v1alpha1.NetworkAppStatus) {
			r.log("network child", "phase", string(a.Phase), "message", a.Message)
		})
		if err != nil {
			return err
		}
		rec, err := r.childRecord(netLayout, app.Instance)
		r.instanceDir = rec.Layout.StateDir
		if err != nil {
			return err
		}
		r.inv.Instance, r.summary.Instance, r.summary.KcpURL = app.Instance, app.Instance, rec.KcpURL
		kubeconfig, workspace = rec.AdminKubeconfig, rec.BaseWorkspace
	default:
		vals := r.vals.With("instance", r.inv.Name).With("profile", bootstrap.ProfileQuick).
			With("state-dir", request.Dir(r.stateRoot, r.inv.Name))
		up, code := bringUp(ctx, r.a, vals, false)
		if code != exitOK {
			return fmt.Errorf("the invocation's kcp did not come up (exit %d)", code)
		}
		r.standalone, r.instanceDir = up.layout, up.layout.StateDir
		r.inv.Instance, r.summary.Instance, r.summary.KcpURL = up.Instance, up.Instance, up.KcpURL
		kubeconfig = up.AdminKubeconfig
	}
	k, err := request.OpenKcp(kubeconfig, workspace)
	if err != nil {
		return err
	}
	r.kcp = k
	if r.inv.Mode == request.ModeStandalone && fresh {
		if err := k.ApplyRoot(ctx, r.root); err != nil {
			return fmt.Errorf("applying the MarketRequester root object: %w", err)
		}
	}
	r.summary.KcpColdSeconds = time.Since(started).Seconds()
	r.summary.KcpRunning = true
	r.log("the invocation's kcp runs", "kcp", r.summary.KcpURL, "workspace", workspace, "instance", r.inv.Instance, "coldSeconds", r.summary.KcpColdSeconds)
	return request.WriteInvocation(r.inv)
}

func (r *requestRun) stop(ctx context.Context, remove bool) {
	r.keepLogs()
	switch r.inv.Mode {
	case request.ModeNetwork:
		_, netRec, err := r.networkLayout()
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
			return
		}
		if remove {
			err = request.RemoveFromNetwork(ctx, netRec.AdminKubeconfig, r.inv.Name)
		} else {
			err = request.SetStopped(ctx, netRec.AdminKubeconfig, r.inv.Name, true)
		}
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
			return
		}
		if !remove {
			_, err = request.WaitNetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name, r.networkTimeout, func(a v1alpha1.NetworkAppStatus) bool {
				return a.Message == appctl.StoppedMessage
			}, nil)
		} else {
			for deadline := time.Now().Add(r.networkTimeout); ; {
				_, found, gerr := request.NetworkApp(ctx, netRec.AdminKubeconfig, r.inv.Name)
				if gerr == nil && !found {
					break
				}
				if time.Now().After(deadline) {
					err = fmt.Errorf("the market network still lists %s", r.inv.Name)
					break
				}
				time.Sleep(request.Poll)
			}
		}
		if err != nil {
			r.a.log.Warn("stopping the invocation", "err", err.Error())
		}
	default:
		l := r.standalone
		if l.ID == "" {
			vals := r.vals.With("instance", r.inv.Name).With("state-dir", request.Dir(r.stateRoot, r.inv.Name))
			var err error
			if l, err = r.a.layoutOf(r.inv.Name, vals); err != nil {
				r.a.log.Warn("stopping the invocation", "err", err.Error())
				return
			}
		}
		d := downInstance(ctx, r.a, l, !remove)
		if len(d.Remaining) > 0 {
			r.a.log.Warn("processes of the invocation's kcp remain", "pids", fmt.Sprint(d.Remaining))
		}
	}
	r.summary.KcpRunning = false
	r.log("the invocation's kcp is stopped", "stateKept", !remove)
}

func (r *requestRun) writeSummary(c *v1alpha1.ComputeContract) {
	r.summary.UpdatedAt = time.Now().UTC()
	if c != nil {
		r.summary.SecretsFetchedAt = c.Status.SecretsFetchedAt
		r.summary.Phase, r.summary.Message, r.summary.SecretsUrl = string(c.Status.Phase), c.Status.Message, c.Status.SecretsUrl
		r.summary.Contract = request.SummaryOf(c)
	}
	if st, err := r.kcp.Root(context.Background()); err == nil && st.Did != "" {
		r.summary.Did = st.Did
	}
	if err := request.WriteSummary(r.stateRoot, r.summary); err != nil {
		r.a.log.Warn("writing summary.json", "err", err.Error())
	}
}

func (r *requestRun) signIn(ctx context.Context) error {
	if err := r.kcp.WaitIdentityObject(ctx, r.networkTimeout); err != nil {
		return err
	}
	ids, err := r.kcp.Identities()
	if err != nil {
		return err
	}
	var in io.Reader
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		in = os.Stdin
	}
	wctx, cancel := context.WithTimeout(ctx, requestIdentityWait)
	defer cancel()
	_, err = appcli.IdentityRenderer{Client: ids, Out: r.a.stderr, In: in}.WaitReady(wctx, appctl.RequesterIdentity)
	return err
}

func contractSettled(c *v1alpha1.ComputeContract) bool {
	switch c.Status.Phase {
	case v1alpha1.ComputeContractHeld, v1alpha1.ComputeContractFailed, v1alpha1.ComputeContractReleased:
		return c.Status.ObservedGeneration == c.Generation || c.Status.ObservedGeneration == 0
	}
	return false
}

func (r *requestRun) progress(c *v1alpha1.ComputeContract) {
	phase := ""
	if c.Status.Contract != nil {
		phase = string(c.Status.Contract.Phase)
	}
	r.log("contract", "phase", string(c.Status.Phase), "contractPhase", phase, "message", c.Status.Message)
}

func (r *requestRun) ssh(ctx context.Context, c *v1alpha1.ComputeContract) (int, error) {
	dp, err := r.a.devExe(ctx, r.vals, "dumbpipe", "dumbpipe-path")
	if err != nil {
		return -1, err
	}
	s := request.SSH{Dumbpipe: dp.Path, Dir: filepath.Join(request.Dir(r.stateRoot, r.inv.Name), request.SSHDir)}
	t, err := s.Target(c)
	if err != nil {
		return -1, err
	}
	r.log("ssh to the guest over its iroh ticket, StrictHostKeyChecking=yes against its reported host keys", "alias", t.Alias,
		"hostKeys", strings.Join(request.SummaryOf(c).HostKeys, ", "))
	held, stop := r.whileHeld(ctx)
	err = s.WaitReady(held, t, requestSSHReady, func(attempt int, msg string) {
		r.log("ssh not ready yet", "attempt", attempt, "err", msg)
	})
	stop()
	if cause := context.Cause(held); err != nil && cause != nil && ctx.Err() == nil {
		return -1, cause
	}
	if err != nil {
		return -1, err
	}
	interactive := false
	if r.exec == "" {
		fi, err := os.Stdin.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			r.log("no --exec and stdin is not a terminal: the guest answered ssh; no session opened")
			return 0, nil
		}
		interactive = true
	}
	code, err := s.Run(ctx, t, r.exec, interactive, os.Stdin, r.a.stdout, r.a.stderr)
	r.log("ssh session ended", "exit", code)
	return code, err
}

func (r *requestRun) whileHeld(ctx context.Context) (context.Context, func()) {
	held, cancel := context.WithCancelCause(ctx)
	go func() {
		for {
			select {
			case <-held.Done():
				return
			case <-time.After(request.Poll):
			}
			c, err := r.kcp.Contract(held)
			if err == nil && c.Status.Phase != v1alpha1.ComputeContractHeld {
				cancel(fmt.Errorf("the contract left Held while ssh waited for the guest: phase %s: %s", c.Status.Phase, c.Status.Message))
				return
			}
		}
	}()
	return held, func() { cancel(nil) }
}

func (r *requestRun) release(ctx context.Context) (*v1alpha1.ComputeContract, error) {
	if err := r.kcp.Release(ctx); err != nil {
		return nil, fmt.Errorf("setting spec.release: %w", err)
	}
	r.log("asked the requester to release the contract (spec.release)")
	return r.kcp.WaitContract(ctx, r.contractTimeout, func(c *v1alpha1.ComputeContract) bool {
		return c.Status.Phase == v1alpha1.ComputeContractReleased ||
			(c.Status.Phase == v1alpha1.ComputeContractFailed && c.Spec.Release && c.Status.Contract != nil && c.Status.Contract.Release != nil)
	}, r.progress)
}

func (r *requestRun) carryOn(ctx context.Context) int {
	if err := r.signIn(ctx); err != nil {
		r.a.log.Error("signing in", "err", err.Error())
		c, _ := r.kcp.Contract(context.Background())
		r.writeSummary(c)
		r.stop(context.Background(), false)
		return exitFailed
	}
	c, err := r.kcp.WaitContract(ctx, r.contractTimeout, contractSettled, r.progress)
	if err != nil {
		r.a.log.Error("the contract did not settle", "err", err.Error())
		r.writeSummary(c)
		r.stop(context.Background(), false)
		r.a.printJSON(r.summary)
		return exitFailed
	}
	if c.Status.Phase == v1alpha1.ComputeContractHeld && c.Spec.SecretsRef != nil && c.Status.SecretsFetchedAt == "" {
		r.log("waiting for the guest to fetch its secrets from the requester's secrets server")
		if fc, err := r.kcp.WaitContract(ctx, r.contractTimeout, func(c *v1alpha1.ComputeContract) bool {
			return c.Status.SecretsFetchedAt != "" || c.Status.Phase != v1alpha1.ComputeContractHeld
		}, r.progress); err != nil {
			r.a.log.Error("the guest did not fetch its secrets", "err", err.Error())
		} else {
			c = fc
			r.log("the guest fetched its secrets", "at", c.Status.SecretsFetchedAt)
		}
	}
	code := exitOK
	hasContract := c.Status.Contract != nil && c.Status.Contract.Receipt != nil
	if c.Status.Phase == v1alpha1.ComputeContractHeld && !r.skipSSH {
		ec, err := r.ssh(ctx, c)
		if err != nil {
			r.a.log.Error("ssh", "err", err.Error())
			code = exitFailed
		} else if ec != 0 {
			code = exitFailed
		}
	}
	if c.Status.Phase == v1alpha1.ComputeContractFailed && !hasContract {
		code = exitFailed
	}
	remove := c.Status.Phase == v1alpha1.ComputeContractReleased || !hasContract
	if !r.hold && hasContract && c.Status.Phase != v1alpha1.ComputeContractReleased {
		rc, err := r.release(context.WithoutCancel(ctx))
		if rc != nil {
			c = rc
		}
		if err != nil || c.Status.Phase != v1alpha1.ComputeContractReleased {
			r.a.log.Error("the release did not finish", "err", fmt.Sprint(err), "phase", string(c.Status.Phase))
			code = exitFailed
		} else {
			remove = true
		}
	}
	r.writeSummary(c)
	r.stop(context.Background(), remove)
	r.writeSummary(nil)
	r.a.printJSON(r.summary)
	return code
}

func runRequest(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	pos, pass := splitWords(rest)
	if len(pos) > 0 {
		return a.fail(exitRefused, "run request: unexpected %q; request-vm-ssh options go after --", strings.Join(pos, " "))
	}
	for _, tool := range []string{"ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			return a.fail(exitRefused, "run request: %s is not on PATH; install the OpenSSH client (apt install openssh-client, dnf install openssh-clients)", tool)
		}
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	name := vals.String("name")
	if name == "" {
		name = "req-" + strings.ToLower(time.Now().UTC().Format("20060102t150405"))
	}
	if err := request.CheckName(name); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	if _, err := os.Stat(request.Dir(r.stateRoot, name)); err == nil {
		return a.fail(exitRefused, "run request: invocation %s exists under %s; run request resume|release %s", name, request.Root(r.stateRoot), name)
	}
	network := vals.String("network")
	if err := r.passthrough(pass, network != ""); err != nil {
		return a.fail(exitRefused, "run request: %v", err)
	}
	var bundle []byte
	if r.secrets != "" {
		if bundle, err = readSecrets(r.secrets); err != nil {
			return a.fail(exitRefused, "run request: --secrets: %v", err)
		}
	}
	r.inv = request.Invocation{Name: name, Mode: request.ModeStandalone, Network: network, StateRoot: r.stateRoot, CreatedAt: time.Now().UTC()}
	if network != "" {
		r.inv.Mode = request.ModeNetwork
	}
	r.summary = request.Summary{Name: name, Mode: r.inv.Mode, Network: network, CreatedAt: r.inv.CreatedAt}
	if !r.skipSSH {
		pub, _, err := request.EnsureKey(ctx, filepath.Join(request.Dir(r.stateRoot, name), request.SSHDir))
		if err != nil {
			return a.fail(exitFailed, "run request: %v", err)
		}
		r.contract.SshAuthorizedKeys = append([]string{pub}, r.contract.SshAuthorizedKeys...)
	}
	if err := request.WriteInvocation(r.inv); err != nil {
		return a.fail(exitFailed, "run request: %v", err)
	}
	r.log("a fresh kcp for this invocation", "mode", r.inv.Mode, "network", network)
	if err := r.start(ctx, true); err != nil {
		r.writeSummaryQuiet()
		r.stop(context.Background(), false)
		return a.fail(exitFailed, "run request: %v", err)
	}
	if bundle != nil {
		ref, err := r.kcp.PutSecrets(ctx, bundle, r.networkTimeout)
		if err != nil {
			r.stop(context.Background(), false)
			return a.fail(exitFailed, "run request: %v", err)
		}
		r.contract.SecretsRef = &ref
		r.log("the secrets bundle is in the invocation's own OpenBao", "mount", ref.Mount, "path", ref.Path)
	}
	if err := r.kcp.CreateContract(ctx, r.contract, r.networkTimeout); err != nil {
		r.stop(context.Background(), false)
		return a.fail(exitFailed, "run request: %v", err)
	}
	return r.carryOn(ctx)
}

func (r *requestRun) writeSummaryQuiet() {
	if err := request.WriteSummary(r.stateRoot, r.summary); err != nil {
		r.a.log.Warn("writing summary.json", "err", err.Error())
	}
}

func (a *app) reopenRequest(ctx context.Context, vals clicfg.Values, rest []string, verb string) (*requestRun, int) {
	pos, pass := splitWords(rest)
	if len(pos) != 1 || len(pass) > 0 {
		return nil, a.fail(exitRefused, "run request %s <name>: one invocation name, no request-vm-ssh options", verb)
	}
	r, err := newRequestRun(a, vals)
	if err != nil {
		return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
	}
	inv, err := request.ReadInvocation(r.stateRoot, pos[0])
	if err != nil {
		return nil, a.fail(exitRefused, "run request %s: %v", verb, err)
	}
	r.inv = inv
	if s, err := request.ReadSummary(r.stateRoot, inv.Name); err == nil {
		r.summary = s
	} else {
		r.summary = request.Summary{Name: inv.Name, Mode: inv.Mode, Network: inv.Network, Instance: inv.Instance, CreatedAt: inv.CreatedAt}
	}
	r.log("starting the invocation's kcp again from its state dir", "verb", verb)
	if err := r.start(ctx, false); err != nil {
		r.stop(context.Background(), false)
		return nil, a.fail(exitFailed, "run request %s: %v", verb, err)
	}
	return r, exitOK
}

func runRequestResume(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.reopenRequest(ctx, vals, rest, "resume")
	if r == nil {
		return code
	}
	return r.carryOn(ctx)
}

func runRequestRelease(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	r, code := a.reopenRequest(ctx, vals, rest, "release")
	if r == nil {
		return code
	}
	c, err := r.release(ctx)
	if err != nil || c == nil || c.Status.Phase != v1alpha1.ComputeContractReleased {
		phase := ""
		if c != nil {
			phase = string(c.Status.Phase)
		}
		r.a.log.Error("the release did not finish", "err", fmt.Sprint(err), "phase", phase)
		r.writeSummary(c)
		r.stop(context.Background(), false)
		r.writeSummary(nil)
		r.a.printJSON(r.summary)
		return exitFailed
	}
	r.writeSummary(c)
	r.stop(context.Background(), true)
	r.writeSummary(nil)
	r.a.printJSON(r.summary)
	return exitOK
}

func runRequestList(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	root, err := devinstance.StateRoot(vals.String("state-dir"), a.env)
	if err != nil {
		return a.fail(exitRefused, "run request list: %v", err)
	}
	list, err := request.List(root)
	if err != nil {
		return a.fail(exitFailed, "run request list: %v", err)
	}
	if vals.Bool("json") {
		a.printJSON(list)
		return exitOK
	}
	for _, s := range list {
		c := request.ContractSummary{}
		if s.Contract != nil {
			c = *s.Contract
		}
		where := s.Mode
		if s.Network != "" {
			where = "network " + s.Network
		}
		fmt.Fprintf(a.stdout, "%-24s %-20s %-10s kcp=%-5t winner=%s guest=%s %s\n", s.Name, where, orDash(s.Phase), s.KcpRunning,
			orDash(c.WinnerDid), orDash(c.VMAddress), s.Message)
	}
	if len(list) == 0 {
		fmt.Fprintln(a.stdout, "no invocations under", request.Root(root))
	}
	return exitOK
}

func (r *requestRun) keepLogs() {
	if r.instanceDir == "" {
		return
	}
	out := filepath.Join(request.Dir(r.stateRoot, r.inv.Name), "logs")
	runs, _ := filepath.Glob(filepath.Join(r.instanceDir, "denokcp", "runs", "pod-*"))
	files := []string{filepath.Join(r.instanceDir, "logs", "core.log")}
	for _, run := range runs {
		for _, f := range []string{"stdout.txt", "stderr.txt"} {
			files = append(files, filepath.Join(run, f))
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(r.instanceDir, f)
		if err != nil {
			continue
		}
		dst := filepath.Join(out, strings.ReplaceAll(rel, string(filepath.Separator), "_"))
		if err := os.MkdirAll(out, 0o700); err == nil {
			os.WriteFile(dst, b, 0o600)
		}
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
