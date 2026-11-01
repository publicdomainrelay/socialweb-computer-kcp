package checkhostmain

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/foldmain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostcontract"
)

const usage = `socialweb-computer check-host - probe the production host contract as the account

  socialweb-computer check-host -role gateway|bidder|requester -state-dir <state> [-pasta <path>] [-asset-cache <dir>] [-json]

install runs it as the account after it creates the account. Each check names its fix (docs/host-contract.md).

-probe-exec, -probe-hold and -probe-userns are the probes the contract starts from a copy of this binary.

Exit status is 0 when the contract holds, 1 when a check fails, and 2 on a usage error.
`

func Main(ctx context.Context, inv foldmain.Invocation) int {
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.SetOutput(inv.Stderr)
	probeExec := fs.Bool("probe-exec", false,
		"print one line and exit 0, so this binary can be executed as a probe from a copied location")
	role := fs.String("role", "",
		"gateway | bidder: probe the production host contract as the account")
	stateDir := fs.String("state-dir", inv.Env("SOCIALWEB_STATE_DIR"),
		"with -role: the account's state dir (probe output, POSIX ACL check)")
	pasta := fs.String("pasta", inv.Env("SOCIALWEB_PASTA_PATH"),
		"with -role bidder in a dev build: the pasta executable (a release build extracts the embedded one)")
	assetCache := fs.String("asset-cache", inv.Env("SOCIALWEB_ASSET_CACHE"),
		"with -role: the asset extraction root (<state-dir>/runtime when unset)")
	asJSON := fs.Bool("json", false, "with -role: print the results as JSON")
	probeHold := fs.Bool("probe-hold", false, "the probe unit's main process: block until stopped")
	probeUserns := fs.Bool("probe-userns", false, "the throwaway user namespace's child: print its maps once released")
	fs.Usage = func() {
		fmt.Fprint(inv.Stderr, usage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(inv.Args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	switch {
	case *probeExec:
		fmt.Fprintln(inv.Stdout, "check-host exec probe: this binary ran from where it was placed")
		return 0
	case *probeHold:
		<-ctx.Done()
		return 0
	case *probeUserns:
		return hostcontract.UsernsChild(os.Stdin, inv.Stdout)
	case *role != "":
		return runContract(ctx, inv, *role, *stateDir, *pasta, *assetCache, *asJSON)
	}
	fs.Usage()
	return 2
}

func runContract(ctx context.Context, inv foldmain.Invocation, roleName, stateDir, pasta, assetCache string, asJSON bool) int {
	role, err := hostcontract.ParseRole(roleName)
	if err != nil {
		fmt.Fprintln(inv.Stderr, err)
		return 2
	}
	if stateDir == "" {
		fmt.Fprintln(inv.Stderr, "check-host -role: -state-dir (SOCIALWEB_STATE_DIR) names the account's state dir; install writes it into the account's config")
		return 2
	}
	if assetCache == "" {
		assetCache = filepath.Join(stateDir, "runtime")
	}
	results := hostcontract.Evaluate(hostcontract.Gather(ctx, hostcontract.Options{
		Role: role, StateDir: stateDir, Self: inv.Self(), PastaPath: pasta, AssetCache: assetCache, Getenv: inv.Env,
	}))
	hostcontract.Write(inv.Stdout, results, asJSON)
	if failed := hostcontract.Failed(results); len(failed) > 0 {
		fmt.Fprintf(inv.Stderr, "check-host: the %s host contract is not met: %s\n", role, strings.Join(failed, ", "))
		return 1
	}
	return 0
}
