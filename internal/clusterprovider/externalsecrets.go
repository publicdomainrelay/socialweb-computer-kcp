package clusterprovider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const externalSecretsReadyMark = "Starting workers"

const externalSecretsReadyController = `"controller":"externalsecret"`

var ExternalSecretsComponents = []procgroup.Component{
	procgroup.ComponentExternalSecretsWebhook, procgroup.ComponentExternalSecrets,
}

var ExternalSecretsPurposes = []portallocator.Purpose{
	portallocator.PurposeExternalSecretsHealth,
	portallocator.PurposeExternalSecretsWebhook,
	portallocator.PurposeExternalSecretsWebhookHealth,
}

var ErrNoExternalSecrets = errors.New(
	"clusterprovider: this Stack runs the External Secrets controller beside the control plane and " +
		"no ExternalSecrets config names the binary and the pinned bundle. The controller's CRDs, its " +
		"ClusterRole and its webhook registration come from that bundle, and a controller started " +
		"against a tenant that serves none of its kinds fails its own start")

var ErrNoExternalSecretsSeam = errors.New(
	"clusterprovider: the External Secrets pair is in this Stack and k3s is not. The pair dials the " +
		"tenant apiserver with a credential minted from the cluster's CAs, so with no apiserver in " +
		"the group there is nothing for it to reconcile against")

var ErrExternalSecretsKubeconfigUnwritable = errors.New(
	"clusterprovider: the External Secrets controller's minted tenant kubeconfig could not be put on " +
		"disk before its step ran. The controller and its webhook take that file as their only " +
		"credential, and controller-runtime exits at start without one")

type ExternalSecrets struct {
	Binary string

	Bundle string

	LegacyNamespace string

	WebhookCAName string

	Install func(ctx context.Context, o ExternalSecretsInstall) error
}

type ExternalSecretsInstall struct {
	StateDir string

	Cluster string

	APIServer string

	DataDir string

	WebhookPort int

	Bundle string

	LegacyNamespace string
}

func (cfg StackConfig) runsExternalSecrets() bool {
	return slices.ContainsFunc(ExternalSecretsComponents, cfg.Stack.Has)
}

func (cfg StackConfig) WithoutUnleasedExternalSecrets(l materialiser.Leases) StackConfig {
	if !cfg.runsExternalSecrets() {
		return cfg
	}
	for _, p := range ExternalSecretsPurposes {
		if _, ok := l[p]; ok {
			continue
		}
		kept := make(Stack, 0, len(cfg.Stack))
		for _, c := range cfg.Stack {
			if c == procgroup.ComponentExternalSecrets || c == procgroup.ComponentExternalSecretsWebhook {
				continue
			}
			kept = append(kept, c)
		}
		cfg.Stack = kept
		return cfg
	}
	return cfg
}

func (cfg StackConfig) WithoutUnleased(l materialiser.Leases) StackConfig {
	return cfg.WithoutUnleasedExternalSecrets(l)
}

func externalSecretsSpec(cfg StackConfig, l materialiser.Leases, cluster string) (clusterstack.ExternalSecrets, error) {
	if cfg.ExternalSecrets == nil || cfg.ExternalSecrets.Binary == "" || cfg.ExternalSecrets.Bundle == "" {
		return clusterstack.ExternalSecrets{}, fmt.Errorf("%w (cluster %s)", ErrNoExternalSecrets, cluster)
	}
	health, err := leasePort(l, portallocator.PurposeExternalSecretsHealth, cluster)
	if err != nil {
		return clusterstack.ExternalSecrets{}, err
	}
	webhook, err := leasePort(l, portallocator.PurposeExternalSecretsWebhook, cluster)
	if err != nil {
		return clusterstack.ExternalSecrets{}, err
	}
	webhookHealth, err := leasePort(l, portallocator.PurposeExternalSecretsWebhookHealth, cluster)
	if err != nil {
		return clusterstack.ExternalSecrets{}, err
	}
	return clusterstack.ExternalSecrets{
		Binary:        cfg.ExternalSecrets.Binary,
		HealthPort:    health,
		WebhookPort:   webhook,
		WebhookHealth: webhookHealth,
	}, nil
}

func ExternalSecretsWebhookURL(port int) string {
	return "https://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func prepareExternalSecrets(steps []procgroup.Step, cfg StackConfig, spec clusterstack.Spec, cluster, apiserver string) ([]procgroup.Step, error) {
	if !cfg.Stack.Has(procgroup.ComponentK3s) {
		return nil, fmt.Errorf("%w (stack %v, cluster %s)", ErrNoExternalSecretsSeam, cfg.Stack, cluster)
	}
	idx, ctlIdx := -1, -1
	for i, s := range steps {
		switch s.Name {
		case procgroup.ComponentExternalSecretsWebhook:
			idx = i
		case procgroup.ComponentExternalSecrets:
			ctlIdx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("clusterprovider: the External Secrets controller of %s is in the Stack "+
			"without its webhook, and the webhook step is the one that installs the tenant half both "+
			"read: the CRDs, the ClusterRole binding and the webhook registration", cluster)
	}
	es := *cfg.ExternalSecrets
	o := ExternalSecretsInstall{StateDir: cfg.StateDir, Cluster: cluster, APIServer: apiserver, DataDir: spec.DataDir,
		WebhookPort: spec.ExternalSecrets.WebhookPort, Bundle: es.Bundle, LegacyNamespace: es.LegacyNamespace}
	install := func(ctx context.Context) error {
		if es.Install == nil {
			return fmt.Errorf("%w (cluster %s)", ErrNoExternalSecrets, cluster)
		}
		return es.Install(ctx, o)
	}
	var once sync.Once
	var installErr error
	shared := func(ctx context.Context, ps procgroup.Spec) (procgroup.Spec, error) {
		once.Do(func() { installErr = install(ctx) })
		return ps, installErr
	}
	steps[idx].Prepare = shared
	if ctlIdx >= 0 {
		steps[ctlIdx].Prepare = shared
	}
	return steps, nil
}

func externalSecretsEnv(spec clusterstack.Spec) []string {
	return []string{"KUBECONFIG=" + clusterstack.ExternalSecretsKubeconfig(spec.DataDir)}
}

func httpReady(ctx context.Context, url string) (bool, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Sprintf("%s answered HTTP %d", url, resp.StatusCode)
	}
	return true, ""
}

func (cfg StackConfig) waitExternalSecretsWebhook(ctx context.Context, spec clusterstack.Spec, p procgroup.Member) error {
	caName := ""
	if cfg.ExternalSecrets != nil {
		caName = cfg.ExternalSecrets.WebhookCAName
	}
	readyz := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.ExternalSecrets.WebhookHealth)) + "/readyz"
	serving := net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.ExternalSecrets.WebhookPort))
	last := "no answer"
	return poll(ctx, func(ctx context.Context) (bool, error) {
		ok, why := httpReady(ctx, readyz)
		if !ok {
			last = why
			return false, nil
		}
		if err := dialWebhookAsTheApiserverWould(spec, caName, serving); err != nil {
			last = err.Error()
			return false, nil
		}
		return true, nil
	}, func() error {
		return &errNotReady{
			what: "the External Secrets webhook. Its readyz answers once its serving pair verifies, and " +
				"the gate then dials " + serving + " verifying the minted CA for 127.0.0.1, which is " +
				"exactly what the tenant apiserver does when it calls the registered URL",
			last: last + "; output tail: " + tail(p.Output()),
		}
	})
}

func dialWebhookAsTheApiserverWould(spec clusterstack.Spec, caName, addr string) error {
	caPEM, err := os.ReadFile(clusterstack.ExternalSecretsCertDir(spec.DataDir) + "/" + caName)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("the webhook CA on disk is not PEM")
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 2 * time.Second}, "tcp", addr,
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err != nil {
		return err
	}
	return conn.Close()
}

func waitExternalSecretsController(ctx context.Context, spec clusterstack.Spec, p procgroup.Member) error {
	readyz := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.ExternalSecrets.HealthPort)) + "/readyz"
	last := "no answer"
	return poll(ctx, func(ctx context.Context) (bool, error) {
		ok, why := httpReady(ctx, readyz)
		if !ok {
			last = why
			return false, nil
		}
		if !externalSecretsWorkersStarted(p.Output()) {
			last = "readyz answers and the externalsecret controller has not started its workers"
			return false, nil
		}
		return true, nil
	}, func() error {
		return &errNotReady{
			what: "the External Secrets controller. Its readyz is a ping that answers before any cache " +
				"has synced, so the gate also waits for the externalsecret controller's own " +
				"\"Starting workers\" line, which controller-runtime logs only after that kind's " +
				"informer has synced against the tenant apiserver",
			last: last + "; output tail: " + tail(p.Output()),
		}
	})
}

func externalSecretsWorkersStarted(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if strings.Contains(line, externalSecretsReadyMark) && strings.Contains(line, externalSecretsReadyController) {
			return true
		}
	}
	return false
}
