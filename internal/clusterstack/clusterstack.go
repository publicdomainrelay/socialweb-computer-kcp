package clusterstack

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/nsexec"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

var ErrNotAProcess = errors.New(
	"clusterstack: the cluster's postgres is its own Postgres object, not a member of the cluster's slice")

var ErrMissingTokenFile = errors.New(
	"clusterstack: TokenFile is required; k3s derives its bootstrap storage key from the " +
		"instance token, so a k3s that mints its own breaks the link instance.PlanFor decided")

type Spec struct {
	DataDir string

	Cluster string

	OpenBaoConfig string

	OpenBaoPort int

	OpenBaoTLSCertFile string

	OpenBaoTLSKeyFile string

	OpenBaoPostgresDSN string

	StoreEndpoint string

	KineListen string

	DatastoreEndpoint string

	AdvertiseAddress string

	BindAddress string

	MarketDialled bool

	TLSSANs []string

	AdvertisePort int

	K3sSecurePort int

	K3sApiserverPort int

	K3sControllerManagerPort      int
	K3sSchedulerPort              int
	K3sCloudControllerManagerPort int

	TokenFile string

	Slot string

	ServerDataDir string

	Hostname string

	HostnameExec []string

	K3sExec string

	KineExec string

	FrontExec []string

	FrontControl string

	FrontState string

	CloudControllerManagerExec []string

	ExternalSecrets ExternalSecrets

	ServiceAccountIssuer string
	APIAudiences         []string

	Audit Audit
}

type ExternalSecrets struct {
	Binary string

	HealthPort int

	WebhookPort int

	WebhookHealth int
}

const ExternalSecretsWebhookLookahead = 720 * time.Hour

type Audit struct {
	PolicyPath string

	LogPath string

	MaxSizeMB  int
	MaxAgeDays int
	MaxBackups int
}

const (
	DefaultAuditMaxSizeMB  = 100
	DefaultAuditMaxAgeDays = 0
	DefaultAuditMaxBackups = 0
)

var (
	ErrNoAuditPolicy = errors.New("clusterstack: Audit.PolicyPath is required")
	ErrNoAuditLog    = errors.New("clusterstack: Audit.LogPath is required")
)

func (a Audit) Validate() error {
	switch {
	case a.PolicyPath == "":
		return ErrNoAuditPolicy
	case a.LogPath == "":
		return ErrNoAuditLog
	}
	return nil
}

func (a Audit) argv() []string {
	size, age, backups := a.MaxSizeMB, a.MaxAgeDays, a.MaxBackups
	size = cmp.Or(size, DefaultAuditMaxSizeMB)
	age = cmp.Or(age, DefaultAuditMaxAgeDays)
	backups = cmp.Or(backups, DefaultAuditMaxBackups)
	return []string{
		"--kube-apiserver-arg=audit-policy-file=" + a.PolicyPath,
		"--kube-apiserver-arg=audit-log-path=" + a.LogPath,
		"--kube-apiserver-arg=audit-log-maxsize=" + strconv.Itoa(size),
		"--kube-apiserver-arg=audit-log-maxage=" + strconv.Itoa(age),
		"--kube-apiserver-arg=audit-log-maxbackup=" + strconv.Itoa(backups),
	}
}

var (
	ErrMissingDataDir    = errors.New("clusterstack: DataDir is required")
	ErrMissingEndpoint   = errors.New("clusterstack: StoreEndpoint is required")
	ErrMissingKineListen = errors.New("clusterstack: KineListen is required: kine defaults to " +
		"0.0.0.0:2379, so the first cluster takes it and every later one FATALs")
	ErrMissingDatastoreEndpoint = errors.New("clusterstack: DatastoreEndpoint is required: k3s " +
		"resolves an empty --datastore-endpoint to its own sqlite and starts a NEW cluster, " +
		"silently -- wrong CA, none of the existing objects")
	ErrMissingAdvertise   = errors.New("clusterstack: AdvertiseAddress and AdvertisePort are both required")
	ErrMissingBindAddress = errors.New("clusterstack: BindAddress is required and is NOT the " +
		"advertise address: the apiserver REJECTS a loopback address for the in-cluster kubernetes " +
		"Service endpoint (measured: subsets[0].addresses[0].ip Invalid value: \"127.0.0.1\": may " +
		"not be in the loopback range), so the advertised address is one the host carries while the " +
		"TCP listener stays off the network. Binding what is advertised is how every tenant's " +
		"apiserver ends up on every interface of the host")
	ErrMissingK3sSecurePort = errors.New("clusterstack: K3sSecurePort is required: k3s's " +
		"--https-listen-port defaults to 6443, ONE PORT FOR THE WHOLE HOST, so the first " +
		"cluster takes it and every later one fails at bind")
	ErrMissingK3sControlPlanePort = errors.New("clusterstack: the k3s control-plane " +
		"component secure ports are required: kube-controller-manager, kube-scheduler and " +
		"the cloud-controller-manager default to 10257, 10259 and 10258 on 127.0.0.1 -- one " +
		"set for the whole host")
	ErrMissingCloudControllerManagerBinary = errors.New("clusterstack: " +
		"CloudControllerManagerExec is required: the cloud controller manager is an out-of-cluster " +
		"controller dialling the apiserver with a minted tenant credential, and nothing on PATH or " +
		"derived from the data dir names that binary. A cluster that came up without it would have " +
		"Nodes carrying no providerID and nothing reconciling them")
	ErrMissingFront = errors.New("clusterstack: the apiserver front owns the cluster's apiserver port " +
		"and every k3s slot sits behind it; it needs the core executable, its control socket and its " +
		"state file")
	ErrMissingExternalSecrets = errors.New("clusterstack: the External Secrets controller and its " +
		"webhook run beside the control plane, one pair per cluster, and each needs the pinned " +
		"external-secrets binary and its own leased ports: the upstream defaults (:8080 metrics, " +
		":8081 webhook health, :8082 controller health, 10250 webhook) are one set for the whole " +
		"host, so the second cluster's pair would fail at bind")
)

func (s Spec) Validate() error {
	switch {
	case s.DataDir == "":
		return ErrMissingDataDir
	case s.StoreEndpoint == "":
		return ErrMissingEndpoint
	case s.KineListen == "":
		return ErrMissingKineListen
	case s.DatastoreEndpoint == "":
		return ErrMissingDatastoreEndpoint
	case s.AdvertiseAddress == "" || s.AdvertisePort <= 0 || s.AdvertisePort > 65535:
		return ErrMissingAdvertise
	case s.BindAddress == "":
		return ErrMissingBindAddress
	case s.K3sSecurePort <= 0 || s.K3sSecurePort > 65535:
		return fmt.Errorf("%w (got %d)", ErrMissingK3sSecurePort, s.K3sSecurePort)
	case s.K3sApiserverPort <= 0 || s.K3sApiserverPort > 65535:
		return fmt.Errorf("%w (apiserver port got %d)", ErrMissingK3sSecurePort, s.K3sApiserverPort)
	case s.K3sControllerManagerPort <= 0 || s.K3sControllerManagerPort > 65535:
		return fmt.Errorf("%w (%s got %d)", ErrMissingK3sControlPlanePort,
			"K3sControllerManagerPort", s.K3sControllerManagerPort)
	case s.K3sSchedulerPort <= 0 || s.K3sSchedulerPort > 65535:
		return fmt.Errorf("%w (%s got %d)", ErrMissingK3sControlPlanePort,
			"K3sSchedulerPort", s.K3sSchedulerPort)
	case s.K3sCloudControllerManagerPort <= 0 || s.K3sCloudControllerManagerPort > 65535:
		return fmt.Errorf("%w (%s got %d)", ErrMissingK3sControlPlanePort,
			"K3sCloudControllerManagerPort", s.K3sCloudControllerManagerPort)
	case s.TokenFile == "":
		return ErrMissingTokenFile
	}
	return s.Audit.Validate()
}

func (s Spec) Argv(c procgroup.Component) ([]string, error) {
	switch c {
	case procgroup.ComponentPostgres:
		return nil, ErrNotAProcess
	case procgroup.ComponentOpenBao:
		return s.OpenBaoArgv()
	case procgroup.ComponentApiserverFront:
		return s.FrontArgv()
	case procgroup.ComponentKine:
		return s.KineArgv()
	case procgroup.ComponentK3s:
		return s.ServerArgv()
	case procgroup.ComponentCloudControllerManager:
		return s.CloudControllerManagerArgv()
	case procgroup.ComponentExternalSecrets:
		return s.ExternalSecretsArgv()
	case procgroup.ComponentExternalSecretsWebhook:
		return s.ExternalSecretsWebhookArgv()
	}
	return nil, fmt.Errorf("clusterstack: unknown component %q", c)
}

func (s Spec) CloudControllerManagerArgv() ([]string, error) {
	if len(s.CloudControllerManagerExec) == 0 || s.CloudControllerManagerExec[0] == "" {
		return nil, ErrMissingCloudControllerManagerBinary
	}
	return append([]string(nil), s.CloudControllerManagerExec...), nil
}

func validPort(p int) bool { return p > 0 && p <= 65535 }

func (s Spec) FrontAddress() string {
	return net.JoinHostPort(s.BindAddress, "0")
}

func (s Spec) FrontArgv() ([]string, error) {
	if len(s.FrontExec) == 0 || s.FrontExec[0] == "" || s.FrontControl == "" || s.FrontState == "" ||
		s.BindAddress == "" {
		return nil, ErrMissingFront
	}
	return append(append([]string(nil), s.FrontExec...),
		"--owner", "cluster-"+s.Cluster,
		"--kcp-front-address", s.FrontAddress(),
		"--front-control", s.FrontControl,
		"--front-state", s.FrontState,
		"--front-fd-store",
	), nil
}

func (s Spec) serverDir() string {
	if s.ServerDataDir != "" {
		return s.ServerDataDir
	}
	return s.DataDir
}

func (s Spec) ServerAdminKubeconfig() string { return AdminKubeconfig(s.serverDir()) }

const RancherEtc = "/etc/rancher"

func (s Spec) RancherDir() string { return filepath.Join(s.serverDir(), "rancher") }

func (s Spec) k3sExec() string {
	if s.K3sExec != "" {
		return s.K3sExec
	}
	return "k3s"
}

func (s Spec) kineExec() string {
	if s.KineExec != "" {
		return s.KineExec
	}
	return "kine"
}

func (s Spec) K3sExecutable() string { return s.k3sExec() }

func (s Spec) KineExecutable() string { return s.kineExec() }

func (s Spec) ServerTLSDir() string { return TLSDir(s.serverDir()) }

func (s Spec) ExternalSecretsArgv() ([]string, error) {
	e := s.ExternalSecrets
	if e.Binary == "" || !validPort(e.HealthPort) {
		return nil, fmt.Errorf("%w (binary %q, health port %d)", ErrMissingExternalSecrets, e.Binary, e.HealthPort)
	}
	return []string{
		e.Binary,
		"--live-addr=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(e.HealthPort)),
		"--metrics-addr=0",
		"--enable-leader-election=false",
		"--zap-time-encoding=iso8601",
	}, nil
}

func (s Spec) ExternalSecretsWebhookArgv() ([]string, error) {
	e := s.ExternalSecrets
	if e.Binary == "" || !validPort(e.WebhookPort) || !validPort(e.WebhookHealth) || s.DataDir == "" {
		return nil, fmt.Errorf("%w (binary %q, webhook port %d, webhook health port %d, data dir %q)",
			ErrMissingExternalSecrets, e.Binary, e.WebhookPort, e.WebhookHealth, s.DataDir)
	}
	return []string{
		e.Binary, "webhook",
		"--port=" + strconv.Itoa(e.WebhookPort),
		"--healthz-addr=" + net.JoinHostPort("127.0.0.1", strconv.Itoa(e.WebhookHealth)),
		"--metrics-addr=0",
		"--cert-dir=" + ExternalSecretsCertDir(s.DataDir),
		"--dns-name=localhost",
		"--lookahead-interval=" + ExternalSecretsWebhookLookahead.String(),
		"--check-interval=1h0m0s",
		"--zap-time-encoding=iso8601",
	}, nil
}

const MarketClusterCIDR = "10.42.0.0/16"

const MarketFlannelBackend = "none"

const MarketKubeletAddressTypes = "Hostname"

func (s Spec) podNetworkArgv() []string {
	if !s.MarketDialled {
		return nil
	}
	return []string{
		"--flannel-backend=" + MarketFlannelBackend,
		"--cluster-cidr=" + MarketClusterCIDR,
		"--kube-apiserver-arg=kubelet-preferred-address-types=" + MarketKubeletAddressTypes,
	}
}

func (s Spec) ServerArgv() ([]string, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	argv := []string{
		s.k3sExec(), "server",
		"--disable-agent",
		"--disable=traefik",
		"--data-dir", s.serverDir(),
		"--datastore-endpoint", s.DatastoreEndpoint,
		"--egress-selector-mode", "cluster",
		"--advertise-address", s.AdvertiseAddress,
		"--advertise-port", strconv.Itoa(s.AdvertisePort),
		"--bind-address", s.BindAddress,
		"--https-listen-port", strconv.Itoa(s.K3sSecurePort),
		"--apiserver-port", strconv.Itoa(s.K3sApiserverPort),
		"--token-file", s.TokenFile,
		"--write-kubeconfig", s.ServerAdminKubeconfig(),
	}
	for _, san := range s.TLSSANs {
		argv = append(argv, "--tls-san", san)
	}
	argv = append(argv, s.podNetworkArgv()...)
	argv = append(argv, s.schedulerArgv()...)
	argv = append(argv, s.Audit.argv()...)
	if s.ServiceAccountIssuer != "" {
		argv = append(argv, "--kube-apiserver-arg=service-account-issuer="+s.ServiceAccountIssuer)
	}
	if len(s.APIAudiences) > 0 {
		argv = append(argv, "--kube-apiserver-arg=api-audiences="+strings.Join(s.APIAudiences, ","))
	}
	argv = append(argv,
		"--kube-controller-manager-arg=secure-port="+strconv.Itoa(s.K3sControllerManagerPort),
		"--kube-scheduler-arg=secure-port="+strconv.Itoa(s.K3sSchedulerPort),
		"--kube-cloud-controller-manager-arg=secure-port="+strconv.Itoa(s.K3sCloudControllerManagerPort),
	)
	if s.Hostname != "" && len(s.HostnameExec) > 0 {
		argv = nsexec.Wrap(s.HostnameExec, s.Hostname, []nsexec.Bind{{Source: s.RancherDir(), Target: RancherEtc}}, argv)
	}
	return argv, nil
}

func (s Spec) KineArgv() ([]string, error) {
	if s.StoreEndpoint == "" {
		return nil, ErrMissingEndpoint
	}
	return []string{
		s.kineExec(),
		"--endpoint", s.StoreEndpoint,
		"--listen-address", s.KineListen,
		"--metrics-bind-address=0",
	}, nil
}

func (s Spec) OpenBaoArgv() ([]string, error) {
	if s.OpenBaoConfig == "" {
		return nil, errors.New("clusterstack: OpenBaoConfig is required")
	}
	return []string{"bao", "server", "-config=" + s.OpenBaoConfig}, nil
}

func TLSDir(dataDir string) string { return filepath.Join(dataDir, "server", "tls") }

func CredDir(dataDir string) string { return filepath.Join(dataDir, "server", "cred") }

const AdminKubeconfigName = "admin.kubeconfig"

func AdminKubeconfig(dataDir string) string {
	return filepath.Join(CredDir(dataDir), AdminKubeconfigName)
}

const CloudControllerManagerKubeconfigName = "cloud-controller-manager.kubeconfig"

func CloudControllerManagerKubeconfig(dataDir string) string {
	return filepath.Join(dataDir, CloudControllerManagerKubeconfigName)
}

const ExternalSecretsKubeconfigName = "external-secrets.kubeconfig"

func ExternalSecretsKubeconfig(dataDir string) string {
	return filepath.Join(dataDir, ExternalSecretsKubeconfigName)
}

func ExternalSecretsDir(dataDir string) string { return filepath.Join(dataDir, "external-secrets") }

func ExternalSecretsCertDir(dataDir string) string {
	return filepath.Join(ExternalSecretsDir(dataDir), "webhook-certs")
}

func ExternalSecretsCAKey(dataDir string) string {
	return filepath.Join(ExternalSecretsDir(dataDir), "webhook-ca.key")
}

const NodeSnapshotName = "nodes.json"

func NodeSnapshotPath(dataDir string) string {
	return filepath.Join(dataDir, NodeSnapshotName)
}
