package gatewaykcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sweepkcp"
)

const (
	DefaultBootstrapDID   = "did:web:example.com"
	bootstrapWorkspaceTTL = 2 * time.Minute
)

var (
	workspaceGVR = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}
	identityGVR  = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "atprotoidentities"}
	clusterGVR   = schema.GroupVersionResource{Group: v1alpha1.GroupName, Version: v1alpha1.Version, Resource: "clusters"}
)

func ssa(ctx context.Context, ri dynamic.ResourceInterface, obj map[string]any) error {
	u := &unstructured.Unstructured{Object: obj}
	body, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	force := true
	_, err = ri.Patch(ctx, u.GetName(), types.ApplyPatchType, body, metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func workspace(name, typ string) map[string]any {
	return map[string]any{"apiVersion": "tenancy.kcp.io/v1alpha1", "kind": "Workspace", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"type": map[string]any{"name": typ, "path": kcpclient.RootWorkspace}}}
}

func retry(ctx context.Context, what string, f func() error) error {
	deadline := time.Now().Add(bootstrapWorkspaceTTL)
	for {
		err := f()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("gatewaykcp: bootstrap cluster: %s: %w", what, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Poll):
		}
	}
}

func waitReady(ctx context.Context, ri dynamic.ResourceInterface, name string) error {
	return retry(ctx, "workspace "+name+" Ready", func() error {
		u, err := ri.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if p, _, _ := unstructured.NestedString(u.Object, "status", "phase"); p != "Ready" {
			return fmt.Errorf("phase %q", p)
		}
		return nil
	})
}

func bootstrapCluster(ctx context.Context, kubeconfig, base, did, cluster string, log *slog.Logger) error {
	if did == "" {
		did = DefaultBootstrapDID
	}
	restCfg, err := memberRestConfig(kubeconfig)
	if err != nil {
		return err
	}
	client := func(ws string) (dynamic.Interface, error) { return dynamic.NewForConfig(clusterConfig(restCfg, ws)) }
	account := kcpclient.AccountHash(did)
	root, err := client(kcpclient.RootWorkspace)
	if err != nil {
		return err
	}
	if err := retry(ctx, "account workspace", func() error {
		return ssa(ctx, root.Resource(workspaceGVR), workspace(account, string(kcpclient.AccountWorkspaceType)))
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
	identity := map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "AtprotoIdentity",
		"metadata": map[string]any{"name": kcpclient.IdentityRecordName(did),
			"annotations": map[string]any{sweepkcp.SweepExemptAnnotation: sweepkcp.SweepExemptValue}},
		"spec": map[string]any{"did": did, "scope": string(v1alpha1.IdentityScopeAccount), "revoked": false}}
	if err := retry(ctx, "account identity", func() error { return ssa(ctx, acct.Resource(identityGVR), identity) }); err != nil {
		return err
	}
	if err := retry(ctx, "home workspace", func() error {
		return ssa(ctx, acct.Resource(workspaceGVR), workspace(cluster, string(kcpclient.HomeWorkspaceType)))
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
	obj := map[string]any{"apiVersion": v1alpha1.GroupVersion.String(), "kind": "Cluster",
		"metadata": map[string]any{"name": cluster, "finalizers": []any{v1alpha1.FinalizerCluster}},
		"spec":     map[string]any{"account": did, "running": true}}
	if err := retry(ctx, "cluster", func() error { return ssa(ctx, home.Resource(clusterGVR), obj) }); err != nil {
		return err
	}
	agent, err := kcpclient.ParseServiceAccountRef(appctl.WebAppAccountRef(base))
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

func BootstrapCluster(ctx context.Context, kubeconfig, did, cluster string, log *slog.Logger) error {
	return bootstrapCluster(ctx, kubeconfig, appctl.DefaultWorkspace, did, cluster, log)
}
