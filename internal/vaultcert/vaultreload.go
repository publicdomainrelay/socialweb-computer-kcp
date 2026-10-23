package vaultcert

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostos"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/opt"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portscan"
)

const (
	DefaultVaultReloadSettle = 10 * time.Second
	vaultReloadPoll          = 100 * time.Millisecond
)

var ErrVaultReloadDidNotTake = errors.New("vaultcert: the running vault was asked to reload its " +
	"TLS certificate and still does not present every host asked for")

var ErrNoVaultToReload = errors.New("vaultcert: no process holding the vault's port is this " +
	"cluster's vault")

type ServedVaultTLS struct {
	DataDir string

	Port int

	Hosts []string

	Reload func(ctx context.Context, dataDir string, port int) error

	Settle time.Duration
}

func EnsureServedVaultTLS(ctx context.Context, dataDir string, port int, hosts ...string) (VaultTLS, error) {
	return ServedVaultTLS{DataDir: dataDir, Port: port, Hosts: hosts}.Ensure(ctx)
}

func (s ServedVaultTLS) Ensure(ctx context.Context) (VaultTLS, error) {
	if s.Port <= 0 || s.Port > 65535 {
		return VaultTLS{}, fmt.Errorf("vaultcert: the vault's port %d is not a port, so there is no "+
			"running listener to check the hosts %v against", s.Port, s.Hosts)
	}
	tls, err := EnsureVaultTLSCertificate(s.DataDir, s.Hosts...)
	if err != nil {
		return VaultTLS{}, err
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port))
	first := RequireServedVaultTLS(ctx, addr, s.Hosts...)
	if first == nil {
		return tls, nil
	}
	if !errors.Is(first, ErrVaultServesAnotherLeaf) {
		return VaultTLS{}, first
	}
	reload := s.Reload
	if reload == nil {
		reload = ReloadVault
	}
	if err := reload(ctx, s.DataDir, s.Port); err != nil {
		return VaultTLS{}, fmt.Errorf("%w. Reloading it was refused: %w", first, err)
	}
	settle := s.Settle
	settle = opt.Positive(settle, DefaultVaultReloadSettle)
	deadline := time.Now().Add(settle)
	last := first
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return VaultTLS{}, err
		}
		last = RequireServedVaultTLS(ctx, addr, s.Hosts...)
		if last == nil {
			return tls, nil
		}
		time.Sleep(vaultReloadPoll)
	}
	return VaultTLS{}, fmt.Errorf("%w within %s: %w. A reload that did not happen and one that "+
		"did look identical from the call that sent it, so the served leaf is what is read, and it "+
		"still lacks the host: restart the vault, or the cluster that holds it, and run this pass "+
		"again", ErrVaultReloadDidNotTake, settle, last)
}

func ReloadVault(_ context.Context, dataDir string, port int) error {
	want, err := clusterstack.Spec{OpenBaoConfig: clusterstack.OpenBaoConfigPath(dataDir)}.OpenBaoArgv()
	if err != nil {
		return err
	}
	pids, err := portscan.ListenerPIDs(port)
	if err != nil {
		return fmt.Errorf("vaultcert: finding the process listening on the vault's port %d: %w", port, err)
	}
	var vault []int
	var strangers []string
	for _, pid := range pids {
		argv, err := hostos.Cmdline(pid)
		if err != nil {
			strangers = append(strangers, fmt.Sprintf("pid %d (%v)", pid, err))
			continue
		}
		if isVaultArgv(argv, want) {
			if !slices.Contains(vault, pid) {
				vault = append(vault, pid)
			}
			continue
		}
		strangers = append(strangers, fmt.Sprintf("pid %d %v", pid, argv))
	}
	if len(vault) != 1 {
		return fmt.Errorf("%w: port %d is held by %d processes whose command line is %v and by %v. "+
			"A process that is not this cluster's vault is never signalled, and two that are cannot "+
			"both be this cluster's one listener", ErrNoVaultToReload, port, len(vault), want, strangers)
	}
	if err := hostos.Reload(vault[0]); err != nil {
		return fmt.Errorf("vaultcert: asking the vault at pid %d to reload its certificate: %w", vault[0], err)
	}
	return nil
}

func isVaultArgv(argv, want []string) bool {
	if len(argv) != len(want) || len(argv) == 0 {
		return false
	}
	if filepath.Base(argv[0]) != filepath.Base(want[0]) {
		return false
	}
	return slices.Equal(argv[1:], want[1:])
}
