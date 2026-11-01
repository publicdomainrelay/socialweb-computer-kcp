package gatewaykcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/clusterhome"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	appgateway "github.com/johnandersen777/socialweb-computer-kcp/internal/app/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/providermain"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/tlsfront"
)

const RequesterGatewayDir = "gateway"

func RequesterSpec(base v1alpha1.GatewaySpec, requester, downloads, nodeDownloadBase string) v1alpha1.GatewaySpec {
	spec := base
	spec.Profile = appgateway.ProfileGatewayRequester
	spec.RequesterRef = &commonv1.ObjectRef{Kind: string(v1alpha1.ApplicationKindMarketRequester), Name: requester}
	var opts []v1alpha1.ApplicationOption
	for _, o := range base.Options {
		if o.Name != appgateway.GatewayDownloadsOption && o.Name != appgateway.GatewayNodeDownloadBase {
			opts = append(opts, o)
		}
	}
	if downloads != "" {
		opts = append(opts, v1alpha1.ApplicationOption{Name: appgateway.GatewayDownloadsOption, Value: downloads})
	}
	if nodeDownloadBase != "" {
		opts = append(opts, v1alpha1.ApplicationOption{Name: appgateway.GatewayNodeDownloadBase, Value: nodeDownloadBase})
	}
	spec.Options = opts
	return spec
}

func RunRequester(ctx context.Context, c Config, rec devinstance.Record, did string, applied func()) error {
	if rec.AdminKubeconfig == "" || rec.KcpURL == "" {
		return errors.New("gatewaykcp: the requester's instance record names no kcp")
	}
	if strings.TrimSpace(did) == "" {
		return errors.New("gatewaykcp: a requester's gateway serves the clusters of the requester's DID, and it has none yet")
	}
	if c.Spec.RequesterRef == nil || c.Spec.Profile != appgateway.ProfileGatewayRequester {
		return fmt.Errorf("gatewaykcp: a requester's gateway is profile %s with spec.requesterRef", appgateway.ProfileGatewayRequester)
	}
	c.StateDir = rec.Layout.StateDir
	if err := os.MkdirAll(rec.Layout.KcpDir(), 0o700); err != nil {
		return err
	}
	g := &gateway{c: c, l: rec.Layout, reg: &listeners{addr: map[string]string{}, kind: map[string]string{}}}
	if err := g.before(ctx, rec); err != nil {
		return err
	}
	if applied != nil {
		applied()
	}
	cert, key, err := tlsfront.EnsureSelfSigned(filepath.Join(c.StateDir, RequesterGatewayDir, "tls"))
	if err != nil {
		return err
	}
	env := newEnv(c.Environ)
	if err := g.defaults(env, rec, cert, key); err != nil {
		return err
	}
	env.set("SOCIALWEB_CLUSTERPROVIDER_ACCOUNT", strings.TrimSpace(did))
	if got, want := env.get("SOCIALWEB_CLUSTERPROVIDER_STATE_DIR"), appgateway.ClusterStateDir(c.StateDir); got != want {
		return fmt.Errorf("gatewaykcp: SOCIALWEB_CLUSTERPROVIDER_STATE_DIR is %s; the requester's GatewayAPI reads the clusters under %s", got, want)
	}
	log := c.log()
	if code := providermain.Main(ctx, g.invocation(env, []string{"migrate"}, "migrate")); code != 0 {
		log.Warn("the legacy-key migration did not complete; the clusterprovider runs the same migration itself", "exit", code)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	wg.Go(func() {
		followMemberToken(mctx, g.activeSlotRoot, g.kcp.SlotsFile(), filepath.Join(g.l.KcpDir(), MemberToken), log)
	})
	log.Info("the requester's gateway runs its clusterprovider in this process; its k3s and kine are HostProcesses of the requester's core",
		"workspace", g.ws, "account", did, "unitPrefix", env.get("SOCIALWEB_CLUSTERPROVIDER_UNIT_PREFIX"), "clusters", env.get("SOCIALWEB_CLUSTERPROVIDER_STATE_DIR"))
	err = exitErr(providermain.Main(mctx, g.invocation(env, nil, "serve", "provider")))
	if mctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("gatewaykcp: the requester's clusterprovider: %w", err)
	}
	return errors.New("gatewaykcp: the requester's clusterprovider returned")
}

func WaitRequesterGateway(ctx context.Context, kubeconfig, ws, name string, timeout time.Duration, progress func(v1alpha1.ApplicationStatus)) (*v1alpha1.Gateway, error) {
	restCfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	dc, err := kube.New(clusterConfig(restCfg, ws))
	if err != nil {
		return nil, err
	}
	last := ""
	o, err := kube.Await(ctx, dc.Resource(compose.GatewayGVR), name, timeout, func(o *v1alpha1.Gateway) (bool, error) {
		if o == nil {
			return false, nil
		}
		st := o.Status
		if key := string(st.Phase) + st.Message; key != last && progress != nil {
			last = key
			progress(st)
		}
		if st.ObservedGeneration != o.Generation {
			return false, nil
		}
		if st.Phase == v1alpha1.ApplicationPhaseFailed {
			return false, fmt.Errorf("gatewaykcp: the Gateway %s failed: %s", name, st.Message)
		}
		return st.Phase == v1alpha1.ApplicationPhaseReady, nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) {
		return nil, fmt.Errorf("gatewaykcp: the Gateway %s was not Ready within %s: %s", name, timeout, last)
	}
	return o, err
}

func TenantAdminKubeconfig(clusters, did, cluster string) (string, error) {
	key, err := clusterhome.Key(clusterhome.HomePath(did, cluster))
	if err != nil {
		return "", err
	}
	dir, err := clusterprovider.ClusterDataDir(clusters, key)
	if err != nil {
		return "", err
	}
	return clusterstack.AdminKubeconfig(dir), nil
}

func RequesterCluster(ctx context.Context, kubeconfig, base, did, cluster string, log *slog.Logger) (string, error) {
	if err := bootstrapCluster(ctx, kubeconfig, base, did, cluster, log); err != nil {
		return "", err
	}
	return clusterhome.HomePath(did, cluster), nil
}

func RequesterClusterPhase(ctx context.Context, kubeconfig, did, cluster string) (v1alpha1.ClusterPhase, error) {
	restCfg, err := memberRestConfig(kubeconfig)
	if err != nil {
		return "", err
	}
	home, err := kube.New(clusterConfig(restCfg, clusterhome.HomePath(did, cluster)))
	if err != nil {
		return "", err
	}
	c, err := kube.Get[v1alpha1.Cluster](ctx, home.Resource(clusterGVR), cluster)
	if err != nil {
		return "", err
	}
	return c.Status.Phase, nil
}
