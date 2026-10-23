package gatewaykcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/johnandersen777/socialweb-computer-kcp/api/kcpgvr"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	appgateway "github.com/johnandersen777/socialweb-computer-kcp/internal/app/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kcpapply"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sweepkcp"
)

const (
	DefaultBootstrapDID   = "did:web:example.com"
	bootstrapWorkspaceTTL = 2 * time.Minute
	bootstrapRetryMax     = 5 * time.Second
)

var (
	workspaceGVR = kcpgvr.Workspaces
	identityGVR  = v1alpha1.AtprotoIdentityGVR
	clusterGVR   = v1alpha1.ClusterGVR
)

func ssa(ctx context.Context, r kube.Resource, obj kube.Doc) error {
	_, err := kube.Apply[kube.Doc](ctx, r, obj, kube.ApplyOptions{FieldManager: FieldManager, Force: true})
	return err
}

func workspace(name, typ string) kube.Doc {
	return kube.Doc{"apiVersion": "tenancy.kcp.io/v1alpha1", "kind": "Workspace", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"type": map[string]any{"name": typ, "path": kcpclient.RootWorkspace}}}
}

func applyWorkspace(ctx context.Context, ap *kcpapply.Applier, parent, name, typ string) error {
	body, err := json.Marshal(workspace(name, typ))
	if err != nil {
		return err
	}
	objs, err := manifests.Decode("gatewaykcp bootstrap workspace "+parent+":"+name, body)
	if err != nil {
		return err
	}
	_, err = ap.ApplyObjects(ctx, parent, objs)
	return err
}

func retry(ctx context.Context, what string, f func() error) error {
	deadline := time.Now().Add(bootstrapWorkspaceTTL)
	delay := Poll
	for {
		err := f()
		if err == nil {
			return nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("gatewaykcp: bootstrap cluster: %s: %w", what, err)
		}
		again := time.NewTimer(min(delay, left))
		select {
		case <-ctx.Done():
			again.Stop()
			return ctx.Err()
		case <-again.C:
		}
		delay = min(2*delay, bootstrapRetryMax)
	}
}

func waitReady(ctx context.Context, r kube.Resource, name string) error {
	var last error
	_, err := kube.Await(ctx, r, name, bootstrapWorkspaceTTL, func(ws *tenancyv1alpha1.Workspace) (bool, error) {
		if ws == nil {
			last = apierrors.NewNotFound(workspaceGVR.GroupResource(), name)
			return false, nil
		}
		if p := string(ws.Status.Phase); p != "Ready" {
			last = fmt.Errorf("phase %q", p)
			return false, nil
		}
		return true, nil
	})
	if errors.Is(err, kube.ErrAwaitTimeout) && last != nil {
		return fmt.Errorf("gatewaykcp: bootstrap cluster: %s: %w", "workspace "+name+" Ready", last)
	}
	return err
}

func bootstrapCluster(ctx context.Context, kubeconfig, base, did, cluster string, log *slog.Logger) error {
	did = cmp.Or(did, DefaultBootstrapDID)
	restCfg, err := memberRestConfig(kubeconfig)
	if err != nil {
		return err
	}
	client := func(ws string) (*kube.Client, error) { return kube.New(clusterConfig(restCfg, ws)) }
	ap, err := kcpapply.New(restCfg, kcpapply.Options{FieldManager: FieldManager, WaitTimeout: bootstrapWorkspaceTTL, Log: log})
	if err != nil {
		return err
	}
	account := kcpclient.AccountHash(did)
	root, err := client(kcpclient.RootWorkspace)
	if err != nil {
		return err
	}
	if err := retry(ctx, "account workspace", func() error {
		return applyWorkspace(ctx, ap, kcpclient.RootWorkspace, account, string(kcpclient.AccountWorkspaceType))
	}); err != nil {
		return err
	}
	if err := waitReady(ctx, root.Resource(workspaceGVR), account); err != nil {
		return err
	}
	acct, err := client(kcpclient.AccountPath(did))
	if err != nil {
		return err
	}
	identity := kube.Doc{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AtprotoIdentity",
		"metadata": map[string]any{"name": kcpclient.IdentityRecordName(did),
			"labels": map[string]any{sweepkcp.SweepExemptLabel: sweepkcp.SweepExemptValue}},
		"spec": map[string]any{"did": did, "scope": string(v1alpha1.IdentityScopeAccount), "revoked": false}}
	if err := retry(ctx, "account identity", func() error { return ssa(ctx, acct.Resource(identityGVR), identity) }); err != nil {
		return err
	}
	if err := retry(ctx, "home workspace", func() error {
		return applyWorkspace(ctx, ap, kcpclient.AccountPath(did), cluster, string(kcpclient.HomeWorkspaceType))
	}); err != nil {
		return err
	}
	if err := waitReady(ctx, acct.Resource(workspaceGVR), cluster); err != nil {
		return err
	}
	home, err := client(kcpclient.HomePath(did, cluster))
	if err != nil {
		return err
	}
	obj := kube.Doc{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Cluster",
		"metadata": map[string]any{"name": cluster, "finalizers": []any{v1alpha1.FinalizerCluster}},
		"spec":     map[string]any{"account": did, "running": true}}
	if err := retry(ctx, "cluster", func() error { return ssa(ctx, home.Resource(clusterGVR), obj) }); err != nil {
		return err
	}
	agent, err := kcpclient.ParseServiceAccountRef(appgateway.WebAppAccountRef(base))
	if err != nil {
		return err
	}
	kc, err := kcpclient.New(kcpclient.Options{Host: restCfg.Host, RestConfig: restCfg, NodeClaimAgent: agent})
	if err != nil {
		return err
	}
	if err := retry(ctx, "web app grant on the home", func() error { return kc.GrantAgentHome(ctx, kcpclient.HomePath(did, cluster)) }); err != nil {
		return err
	}
	log.Info("the bootstrap cluster exists", "cluster", cluster, "account", did, "home", kcpclient.HomePath(did, cluster))
	return nil
}

func BootstrapCluster(ctx context.Context, kubeconfig, base, did, cluster string, log *slog.Logger) error {
	base = cmp.Or(base, compose.DefaultWorkspace)
	return bootstrapCluster(ctx, kubeconfig, base, did, cluster, log)
}
