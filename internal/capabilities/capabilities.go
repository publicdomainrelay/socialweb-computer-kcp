package capabilities

import (
	"fmt"
	"strings"
)

type Need string

const (
	NeedHostBackend Need = "host-backend"
	NeedProc        Need = "proc"
	NeedSystemdUser Need = "systemd-user"
	NeedVMRuntime   Need = "vm-runtime"
)

type Verb struct {
	Path    string
	Summary string
	BuiltBy string
	Needs   []Need
	Hidden  bool
}

func (v Verb) Built() bool { return v.BuiltBy == "" }

func (v Verb) Words() []string { return strings.Fields(v.Path) }

const (
	PlanWindows = "plan 0011"
	PlanDarwin  = "plan 0012"
)

var available = map[string]map[Need]bool{
	"linux": {NeedHostBackend: true, NeedProc: true, NeedSystemdUser: true, NeedVMRuntime: true},
}

var needNames = map[Need]string{
	NeedHostBackend: "no placement backend",
	NeedProc:        "no proc placement backend",
	NeedSystemdUser: "no systemd user manager",
	NeedVMRuntime:   "no VM runtime",
}

func planFor(goos string) string {
	switch goos {
	case "windows":
		return PlanWindows
	case "darwin":
		return PlanDarwin
	}
	return "no plan names " + goos
}

func Missing(v Verb, goos, goarch string) (string, bool) {
	for _, n := range v.Needs {
		if !available[goos][n] {
			return fmt.Sprintf("%s: %s on %s/%s in this build (%s)", v.Path, needNames[n], goos, goarch, planFor(goos)), true
		}
	}
	return "", false
}

func Tree() []Verb {
	return append([]Verb{
		{Path: "version", Summary: "print the build, its Go and its target (--json)"},
		{Path: "dev up", Summary: "start (or adopt) an application instance under devsup: kine, a port-0 front and one kcp slot behind it (--profile minimal; --fg binds it to this terminal)", Needs: []Need{NeedProc}},
		{Path: "dev host", Summary: "start (or adopt) the user's parent instance (dev-host): its own bootstrap layer under its own devsup; its root:dev:<instance> workspaces hold each child's HostProcess objects (dev up --parent dev-host)", Needs: []Need{NeedProc}},
		{Path: "dev status", Summary: "the instance record, its devsup units and, for a market network, each child application's market loop and contract steps", Needs: []Need{NeedProc}},
		{Path: "status", Summary: "status [<app>]: each application's root object (phase, status.market: RFPs seen and scoped, declines by reason, bids, wins, active contracts, stream cursors) and each ComputeContract's phase and current step, read from the application's own kcp", Needs: []Need{NeedProc}},
		{Path: "dev down", Summary: "stop every unit of the instance and remove its state (--keep-state keeps it)", Needs: []Need{NeedProc}},
		{Path: "dev ls", Summary: "every dev instance under the state root, with its devsup reachability", Needs: []Need{NeedProc}},
		{Path: "dev logs", Summary: "a unit's log: dev logs <kine-a|front|kcp-a|kcp-b|core|devsup> [--tail N]", Needs: []Need{NeedProc}},
		{Path: "dev env", Summary: "exports for shells and TS tools (KUBECONFIG, kcp URL, ports, leases; --json)", Needs: []Need{NeedProc}},
		{Path: "dev check", Summary: "report the dev host prerequisites; never fixes them", Needs: []Need{NeedProc}},
		{Path: "dev restart", Summary: "dev restart [<instance>] core [--binary path]: replace the dev core, adopting every other unit; dev restart [<instance>] [<app>/][<workspace>:][<kind>/]<component>: restart one component through its kcp object, wait until it is Ready, print the old and new state", Needs: []Need{NeedProc}},
		{Path: "dev roll", Summary: "roll a dev instance's kcp (a tenant cluster rolls with units roll cluster)", Needs: []Need{NeedProc}},
		{Path: "dev roll kcp", Summary: "blue/green kcp slot swap behind the instance's front", Needs: []Need{NeedProc}},
		{Path: "dev gc", Summary: "collect the per-user asset cache (refused while a devsup of this user runs)", Needs: []Need{NeedProc}},
		{Path: "serve gateway kcp", Summary: "the gateway application instance: adopts or starts its kcp store, kine, front and active slot, applies its sets and its Gateway root object, then runs the root vault, the gateway, the TLS front, the clusterprovider and the door", Needs: []Need{NeedHostBackend}},
		{Path: "serve gateway xrpc", Summary: "the XRPC gateway (SOCIALWEB_GATEWAY_* env)"},
		{Path: "serve provider", Summary: "the cluster provider (SOCIALWEB_CLUSTERPROVIDER_* env)"},
		{Path: "run bidder", Summary: "a bidder application instance: applies its MarketBidder root object with --profile prod-qr|market-network|dev-local into the handle's own kcp (--network <market network> makes it a child of that network), renders the sign-in and returns when Ready (--fg stays); hono-bidder options after --", Needs: []Need{NeedHostBackend, NeedVMRuntime}},
		{Path: "run gateway ssh", Summary: "ssh to a guest through the relay", BuiltBy: "plan 0008 M4-RUN-GATEWAY-SSH"},
		{Path: "run request", Summary: "rent a guest through the RFP flow: a fresh kcp per invocation (--network <market network> makes it a child of that network), the requester component, ssh with StrictHostKeyChecking=yes; request-vm-ssh options after --", Needs: []Need{NeedProc}},
		{Path: "run request list", Summary: "every invocation's summary.json; starts no kcp", Needs: []Need{NeedProc}},
		{Path: "run request release", Summary: "run request release <name>: start the invocation's kcp again from its state dir, send vm.delete, then remove the invocation", Needs: []Need{NeedProc}},
		{Path: "run request resume", Summary: "run request resume <name>: start the invocation's kcp again from its state dir and carry its contract on (ssh, then release unless --hold)", Needs: []Need{NeedProc}},
		{Path: "vault apply-root", Summary: "generate the root vault PKI root and write its KV entries as its client gateway (the OpenBaoServer root-vault declares the rest)"},
		{Path: "migrate", Summary: "apply the clusterprovider migrations"},
		{Path: "apply", Summary: "server-side apply manifest sets into kcp workspaces (--set a,b --values file --dry-run; --instance or --kcp-kubeconfig)"},
		{Path: "manifests list", Summary: "list the embedded manifest sets, their phases and the YAML no set applies (--json)"},
		{Path: "manifests render", Summary: "render manifest sets with typed values (--set --values --phase)"},
		{Path: "check-host", Summary: "probe the Linux host contract as the account (-role gateway|bidder -state-dir)", Needs: []Need{NeedSystemdUser}},
		{Path: "install gateway", Summary: "the one explicit root step for a gateway", Needs: []Need{NeedSystemdUser}},
		{Path: "install bidder", Summary: "the one explicit root step for a bidder", Needs: []Need{NeedSystemdUser}},
		{Path: "uninstall gateway", Summary: "undo install gateway", Needs: []Need{NeedSystemdUser}},
		{Path: "uninstall bidder", Summary: "undo install bidder", Needs: []Need{NeedSystemdUser}},
		{Path: "upgrade", Summary: "content-addressed swap of the core binary: --binary, --sha256; self-check, retarget bin/current, restart the core units (as the account)"},
		{Path: "rollback", Summary: "retarget bin/current to bin/previous and restart the core units (as the account)"},
		{Path: "units list", Summary: "list this account's units", Needs: []Need{NeedHostBackend}},
		{Path: "units audit", Summary: "audit VM process trees: only pinned VM-unit asset executables may run in a VM unit (exit 1 naming the unit and exe)", Needs: []Need{NeedHostBackend}},
		{Path: "units gc", Summary: "collect failed and orphaned units", BuiltBy: "M3-CLUSTER-UNITS", Needs: []Need{NeedHostBackend}},
		{Path: "units roll cluster", Summary: "blue/green swap of a tenant cluster's k3s+kine slot behind its apiserver front (a request the cluster provider carries out; --k3s-path, --kine-path for a version change)"},
		{Path: "units roll link", Summary: "replace a tenant cluster link's dumbpipe listener with one on the same iroh key and the other UDP lease (--service, --dumbpipe-path)"},
		{Path: "units roll kcp", Summary: "roll the installed kcp: a new slot beside the old on the same store, the front flips, the old slot stops, the controller lease handed over (--kcp-path for a kcp version change, --planned-outage for an incompatible pair)", Needs: []Need{NeedSystemdUser}},
		{Path: "internal devsup", Summary: "the per-instance proc backend supervisor", Needs: []Need{NeedProc}, Hidden: true},
		{Path: "internal dev-core", Summary: "the dev instance's core: adopts the bootstrap units and records them", Needs: []Need{NeedProc}, Hidden: true},
		{Path: "internal iroh-dial", Summary: "listen on a unix socket and, per connection, run dumbpipe connect to the iroh ticket the connection names (the ssh gateway application's HostProcess)", Hidden: true},
		{Path: "internal ns-exec", Summary: "run a command under a private hostname in its own user+UTS+mount namespace (one-shot exec, no resident code)", Hidden: true},
		{Path: "internal vm start", Summary: "prepare and start one VM unit (spec JSON on stdin; prints its runtime record)", Needs: []Need{NeedHostBackend, NeedVMRuntime}, Hidden: true},
		{Path: "internal vm inspect", Summary: "the verdict of one VM: Running, Lost (a mismatch is stopped now) or Unknown", Needs: []Need{NeedHostBackend, NeedVMRuntime}, Hidden: true},
		{Path: "internal vm stop", Summary: "stop one VM (CtrlAltDel, the grace, then SIGKILL) and remove its work dir", Needs: []Need{NeedHostBackend, NeedVMRuntime}, Hidden: true},
		{Path: "internal vm list", Summary: "the VM units and work dirs of one handle", Needs: []Need{NeedHostBackend, NeedVMRuntime}, Hidden: true},
		{Path: "internal apiserver-front", Summary: "the L4 front of kcp or a tenant apiserver", Hidden: true},
		{Path: "internal hostprocesses", Summary: "the HostProcess supervisor and the OpenBaoServer controller against a kcp the caller names (a market network's bidder vaults until plan 0007 M5-RUN-BIDDER)", Needs: []Need{NeedSystemdUser}, Hidden: true},
		{Path: "internal cloud-controller-manager", Summary: "the tenant CCM, re-exec'd by the cluster provider", Hidden: true},
		{Path: "internal tenantcredential", Summary: "mint a tenant credential (socialweb-tenantcredential)", Hidden: true},
	}, devVerbs...)
}

func Find(args []string) (Verb, []string, bool) {
	var best Verb
	n := 0
	for _, v := range Tree() {
		w := v.Words()
		if len(w) > len(args) || len(w) <= n {
			continue
		}
		match := true
		for i := range w {
			if w[i] != args[i] {
				match = false
				break
			}
		}
		if match {
			best, n = v, len(w)
		}
	}
	return best, args[n:], n > 0
}

func Groups() map[string]bool {
	out := map[string]bool{}
	for _, v := range Tree() {
		w := v.Words()
		for i := 1; i < len(w); i++ {
			out[strings.Join(w[:i], " ")] = true
		}
	}
	return out
}
