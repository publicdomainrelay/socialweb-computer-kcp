package clusterprovider

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

type VaultTLS struct {
	CertFile string

	KeyFile string

	CAPEM []byte
}

var ErrNoVaultTLS = errors.New(
	"clusterprovider: the vault's listener is loopback-only and TLS, and no per-cluster " +
		"certificate source was supplied. A listener with no certificate cannot present one, so " +
		"auth/cert can never answer on it and every guest's and every admin's call fails with a " +
		"TLS connection error that names nothing")

const TokenFileName = "token"

func TokenPath(dataDir string) string { return filepath.Join(dataDir, TokenFileName) }

func DaemonURL(kineListen string) string { return "http://" + kineListen }

var ErrNoStore = errors.New(
	"clusterprovider: no store endpoint for this cluster, and kine is the only thing k3s dials. " +
		"k3s is NOT pointed at the bucket: measured against k3s v1.36.4+k3s1, the S3 form is refused " +
		"at startup with 'unknown driver' and a t4 endpoint makes k3s start its OWN kine against the " +
		"same bucket instead of dialing ours, which is two writers on one store")

type StackConfig struct {
	Stack Stack

	Admin AdminClient

	Open OpenTenant

	WriteKubeconfig KubeconfigWriter

	StateDir string

	StoreEndpoint func(cluster string) (string, error)

	VaultStoreDSN func(cluster string) (string, error)

	AdvertiseAddress string

	BindAddress string

	MarketDialled bool

	VaultTLS func(dataDir string, hosts ...string) (VaultTLS, error)

	TLSSANs []string

	ServiceAccountIssuer string
	APIAudiences         []string

	CloudControllerManagerExec []string

	FrontExec []string

	Slot func(cluster string) SlotState

	HostnameExec []string

	K3sExec string

	KineExec string

	PgDumpExec string

	LookPath func(name string) (string, error)

	LinkSlot func(cluster string, service v1alpha1.ClusterLinkService) LinkRecord

	ExternalSecrets *ExternalSecrets

	AuditPolicyPath string

	Harvest func(ctx context.Context, cluster string) error

	GuardRides bool

	Links *Links

	Revoked *RevokedServiceAccounts
}

type SlotState struct {
	Slot string `json:"slot"`
	K3s  string `json:"k3s,omitempty"`
	Kine string `json:"kine,omitempty"`
}

func SlotHostname(cluster, slot string) string {
	key := strings.ReplaceAll(cluster, ".", "-")
	if len(key) > 12 {
		key = strings.TrimRight(key[:12], "-")
	}
	return "swc-" + key + "-" + slot
}

func (cfg StackConfig) SlotSpec(o materialiser.Observed, l materialiser.Leases, st SlotState) (clusterstack.Spec, error) {
	cluster := o.Cluster.Name
	if cluster == "" {
		return clusterstack.Spec{}, errors.New("clusterprovider: a slot spec was asked for an observation with no cluster name")
	}
	dataDir, err := ClusterDataDir(cfg.StateDir, cluster)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	slot := slotOr(st.Slot)
	sp, err := portallocator.PurposesOfSlot(slot)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	advPort, k3sSecure, err := K3sAPIPorts(l, cluster, slot)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	k3sApiserver, err := leasePort(l, sp.Apiserver, cluster)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	k3sControllerManager, err := leasePort(l, sp.ControllerManager, cluster)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	k3sScheduler, err := leasePort(l, sp.Scheduler, cluster)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	k3sCloudControllerManager, err := leasePort(l, sp.CloudControllerManager, cluster)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	kineLease, ok := l[sp.Kine]
	if !ok {
		return clusterstack.Spec{}, fmt.Errorf("%w: %s (cluster %s)", ErrNoLease, sp.Kine, cluster)
	}
	kineListen := fmt.Sprintf("127.0.0.1:%d", kineLease.Port)

	if cfg.BindAddress == "" {
		return clusterstack.Spec{}, fmt.Errorf("%w (cluster %s)", clusterstack.ErrMissingBindAddress, cluster)
	}
	if cfg.StoreEndpoint == nil {
		return clusterstack.Spec{}, fmt.Errorf("%w (cluster %s)", ErrNoStore, cluster)
	}
	store, err := cfg.StoreEndpoint(cluster)
	if err != nil {
		return clusterstack.Spec{}, err
	}
	if store == "" {
		return clusterstack.Spec{}, fmt.Errorf("%w (cluster %s)", ErrNoStore, cluster)
	}
	k3sExe, err := cfg.resolveExe(st.K3s, cfg.K3sExec, "k3s")
	if err != nil {
		return clusterstack.Spec{}, err
	}
	kineExe, err := cfg.resolveExe(st.Kine, cfg.KineExec, "kine")
	if err != nil {
		return clusterstack.Spec{}, err
	}

	advertise, sans := cfg.announced()
	serverDir := SlotDataDir(dataDir, slot)
	spec := clusterstack.Spec{
		DataDir:                       dataDir,
		Cluster:                       cluster,
		OpenBaoConfig:                 clusterstack.OpenBaoConfigPath(dataDir),
		StoreEndpoint:                 store,
		KineListen:                    kineListen,
		DatastoreEndpoint:             DaemonURL(kineListen),
		AdvertiseAddress:              advertise,
		BindAddress:                   cfg.BindAddress,
		MarketDialled:                 cfg.MarketDialled,
		TLSSANs:                       sans,
		AdvertisePort:                 advPort,
		K3sSecurePort:                 k3sSecure,
		K3sApiserverPort:              k3sApiserver,
		K3sControllerManagerPort:      k3sControllerManager,
		K3sSchedulerPort:              k3sScheduler,
		K3sCloudControllerManagerPort: k3sCloudControllerManager,
		TokenFile:                     TokenPath(dataDir),
		Slot:                          slot,
		ServerDataDir:                 serverDir,
		K3sExec:                       k3sExe,
		KineExec:                      kineExe,
		FrontExec:                     cfg.FrontExec,
		FrontControl:                  FrontControlPath(cfg.StateDir, dataDir, cluster),
		FrontState:                    FrontStatePath(dataDir),
		CloudControllerManagerExec:    cfg.CloudControllerManagerExec,
		ServiceAccountIssuer:          cfg.ServiceAccountIssuer,
		APIAudiences:                  cfg.APIAudiences,
		Audit: clusterstack.Audit{
			PolicyPath: cfg.AuditPolicyPath,
			LogPath:    filepath.Join(serverDir, "audit.log"),
		},
	}
	if len(cfg.HostnameExec) > 0 {
		spec.HostnameExec = cfg.HostnameExec
		spec.Hostname = SlotHostname(cluster, slot)
	}
	if err := WriteKubeSchedulerConfig(spec); err != nil {
		return clusterstack.Spec{}, err
	}
	return spec, nil
}

func (cfg StackConfig) resolveExe(path, configured, name string) (string, error) {
	path = cmp.Or(path, configured)
	path = cmp.Or(path, name)
	if filepath.IsAbs(path) {
		return path, nil
	}
	look := cfg.LookPath
	if look == nil {
		look = exec.LookPath
	}
	p, err := look(path)
	if err != nil {
		return "", fmt.Errorf("clusterprovider: %s is not on this host: %w", path, err)
	}
	return filepath.Abs(p)
}

func StepsFor(cfg StackConfig) materialiser.StepBuilder {
	return func(o materialiser.Observed, l materialiser.Leases) ([]procgroup.Step, error) {
		cluster := o.Cluster.Name
		if cluster == "" {
			return nil, errors.New("clusterprovider: StepsFor was given an observation with no cluster name")
		}
		dataDir, err := ClusterDataDir(cfg.StateDir, cluster)
		if err != nil {
			return nil, err
		}
		spec, err := cfg.SlotSpec(o, l, cfg.slotOf(cluster))
		if err != nil {
			return nil, err
		}
		advPort, advertise := spec.AdvertisePort, spec.AdvertiseAddress

		if cfg.runsExternalSecrets() {
			spec.ExternalSecrets, err = externalSecretsSpec(cfg, l, cluster)
			if err != nil {
				return nil, err
			}
		}

		if cfg.Stack.Has(procgroup.ComponentOpenBao) {
			baoPort, err := leasePort(l, portallocator.PurposeOpenBao, cluster)
			if err != nil {
				return nil, err
			}
			spec.OpenBaoPort = baoPort
			if cfg.VaultTLS == nil {
				return nil, fmt.Errorf("%w (cluster %s)", ErrNoVaultTLS, cluster)
			}
			vaultTLS, err := cfg.VaultTLS(dataDir, advertise, "127.0.0.1")
			if err != nil {
				return nil, err
			}
			spec.OpenBaoTLSCertFile = vaultTLS.CertFile
			spec.OpenBaoTLSKeyFile = vaultTLS.KeyFile
			if cfg.VaultStoreDSN == nil {
				return nil, fmt.Errorf("%w (cluster %s)", ErrNoVaultStore, cluster)
			}
			dsn, err := cfg.VaultStoreDSN(cluster)
			if err != nil {
				return nil, err
			}
			if dsn == "" {
				return nil, fmt.Errorf("%w (cluster %s)", ErrNoVaultStore, cluster)
			}
			spec.OpenBaoPostgresDSN = dsn
		}

		steps, err := cfg.StepsFrom(spec, l)
		if err != nil {
			return nil, err
		}
		steps = cfg.Revoked.beforeK3s(steps, o, spec)
		if cfg.GuardRides {
			steps, err = HarvestAfterK3s(steps, cfg.Harvest, cluster)
			if err != nil {
				return nil, err
			}
		}
		steps, err = hardenAfterK3s(steps, cfg.Admin, cluster, APIServerURL(advPort))
		if err != nil {
			return nil, err
		}
		if cfg.Stack.Has(procgroup.ComponentCloudControllerManager) {
			steps, err = prepareCloudControllerManager(steps, cfg, cluster, APIServerURL(advPort))
			if err != nil {
				return nil, err
			}
		}
		if cfg.runsExternalSecrets() {
			steps, err = prepareExternalSecrets(steps, cfg, spec, cluster, APIServerURL(advPort))
			if err != nil {
				return nil, err
			}
		}
		if cfg.Links != nil {
			steps, err = linksAfterK3s(steps, cfg, cluster, dataDir, advPort, spec.OpenBaoPort)
			if err != nil {
				return nil, err
			}
		}
		return steps, nil
	}
}

func (cfg StackConfig) announced() (string, []string) {
	return cfg.AdvertiseAddress, []string{cfg.AdvertiseAddress}
}

func K3sAPIPorts(l materialiser.Leases, cluster, slot string) (advertisePort, listenPort int, err error) {
	sp, err := portallocator.PurposesOfSlot(slot)
	if err != nil {
		return 0, 0, err
	}
	if advertisePort, err = leasePort(l, portallocator.PurposeK3sSecure, cluster); err != nil {
		return 0, 0, err
	}
	if listenPort, err = leasePort(l, sp.Secure, cluster); err != nil {
		return 0, 0, err
	}
	return advertisePort, listenPort, nil
}

const SlotsDirName = "slots"

func SlotDataDir(dataDir, slot string) string {
	if slot == "" || slot == ActiveSlot {
		return dataDir
	}
	return filepath.Join(dataDir, SlotsDirName, slot)
}

func (cfg StackConfig) slotOf(cluster string) SlotState {
	if cfg.Slot != nil {
		if s := cfg.Slot(cluster); s.Slot != "" {
			return s
		}
	}
	return SlotState{Slot: ActiveSlot}
}

func leasePort(l materialiser.Leases, p portallocator.Purpose, cluster string) (int, error) {
	lease, ok := l[p]
	if !ok {
		return 0, fmt.Errorf("%w: %s (cluster %s)", ErrNoLease, p, cluster)
	}
	return lease.Port, nil
}
