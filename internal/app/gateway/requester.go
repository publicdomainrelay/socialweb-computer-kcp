package gateway

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/marketrequester"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
)

const (
	ProfileGatewayRequester = "requester"
	GatewayDownloadsOption  = "downloads-dir"
	GatewayNodeDownloadBase = "node-download-base"
	requesterSetPhase       = "rbac"
	requesterNodeVaultMount = marketrequester.RequesterVaultClient
)

func requesterProfile(o *v1alpha1.Gateway) bool {
	return o.Spec.RequesterRef != nil || o.Spec.Profile == ProfileGatewayRequester
}

func ClusterStateDir(stateRoot string) string { return filepath.Join(stateRoot, "clusterprovider") }

func (p *pass) requesterRoot(ctx context.Context) (*v1alpha1.MarketRequester, string, bool, error) {
	o, r := p.o, p.r
	ref := o.Spec.RequesterRef
	switch {
	case o.Spec.Profile != ProfileGatewayRequester:
		return nil, fmt.Sprintf("spec.requesterRef needs profile %s, not %q", ProfileGatewayRequester, o.Spec.Profile), true, nil
	case ref == nil || strings.TrimSpace(ref.Name) == "":
		return nil, fmt.Sprintf("profile %s needs spec.requesterRef naming the MarketRequester whose Requester rents the nodes", ProfileGatewayRequester), true, nil
	case ref.Kind != "" && ref.Kind != string(v1alpha1.ApplicationKindMarketRequester):
		return nil, fmt.Sprintf("spec.requesterRef names a %s; it names a MarketRequester", ref.Kind), true, nil
	case cmp.Or(strings.TrimSpace(ref.Workspace), r.Workspace) != r.Workspace:
		return nil, fmt.Sprintf("spec.requesterRef names workspace %s; the MarketRequester runs in the Gateway's own workspace %s", ref.Workspace, r.Workspace), true, nil
	}
	mr, err := compose.Get[v1alpha1.MarketRequester](ctx, p.c, r.Workspace, compose.MarketRequesterGVR, ref.Name)
	if apierrors.IsNotFound(err) {
		return nil, "waiting for the MarketRequester " + ref.Name, false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if mr.Status.Phase != v1alpha1.ApplicationPhaseReady || strings.TrimSpace(mr.Status.Did) == "" {
		return mr, fmt.Sprintf("waiting for the MarketRequester %s to be Ready with a did (%s: %s)", ref.Name, mr.Status.Phase, mr.Status.Message), false, nil
	}
	return mr, "", false, nil
}

func (p *pass) requesterLinks() []compose.Link {
	return []compose.Link{compose.NodeClaimVaultLink(p.r.Workspace, marketrequester.RequesterVaultServer, requesterNodeVaultMount+"/contracts/*")}
}

func (p *pass) requesterDesired(mr *v1alpha1.MarketRequester) ([]compose.Desired, error) {
	c, o, ws := p.c, p.o, p.r.Workspace
	network := mr.Spec.Network
	stateRoot := c.StateRoot()
	clusterState := ClusterStateDir(stateRoot)
	downloads := strings.TrimSpace(compose.OptionValue(o.Spec.Options, GatewayDownloadsOption))
	if downloads == "" && p.ctl.web != nil {
		downloads = p.ctl.web.DownloadsDir
	}
	storage := &v1alpha1.ComponentStorage{BlobsDir: downloads, CertsDir: filepath.Join(clusterState, clusterstack.PublicCADirName)}
	var required []string
	if downloads != "" {
		required = slices.Clone(GatewayRequiredDownloads)
	}
	var clusterProvider *v1alpha1.GatewayClusterProvider
	if o.Spec.ClusterProvider != nil {
		cp := *o.Spec.ClusterProvider
		clusterProvider = &cp
	}
	market := &v1alpha1.GatewayNodeMarket{HomeWorkspace: p.ws, ContractWorkspace: ws,
		NodePolicy: compose.OptionValue(o.Spec.Options, GatewayNodePolicy), NodePolicyArgs: compose.OptionValue(o.Spec.Options, GatewayNodePolicyArgs),
		InfraRecord: &v1alpha1.GatewayInfraRecord{RepoWorkspace: ws}, VaultMount: requesterNodeVaultMount}
	api := compose.Desired{Kind: compose.ComponentKindGatewayAPI, Name: GatewayAPIComponent, Spec: v1alpha1.GatewayAPISpec{
		ComponentSpec:     compose.NewComponentSpec(compose.ComponentCRStateDir(stateRoot, compose.ComponentKindGatewayAPI, GatewayAPIComponent), network),
		RequesterRef:      compose.ComponentRefFor(compose.ComponentKindRequester, marketrequester.RequesterComponent),
		VaultRef:          compose.ComponentRefFor(compose.KindOpenBaoServer, marketrequester.RequesterVaultServer),
		Storage:           storage,
		ClusterProvider:   clusterProvider,
		ClusterStateDir:   clusterState,
		Audience:          mr.Status.Did,
		PublicOrigin:      strings.TrimRight(strings.TrimSpace(compose.OptionValue(o.Spec.Options, GatewayNodeDownloadBase)), "/"),
		RequiredDownloads: required,
		NodeMarket:        market,
		NodesOnly:         true,
	}}
	out := append([]compose.Desired{api}, compose.HTTPServiceExposures(ws, compose.ComponentKindGatewayAPI, GatewayAPIComponent, "http", network, nil, true, false)...)
	return compose.ApplyOverrides(o.Spec.Components, out)
}

func (p *pass) requester(ctx context.Context) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	mr, msg, failed, err := p.requesterRoot(ctx)
	switch {
	case err != nil:
		return p.retry("the MarketRequester", err)
	case failed:
		return p.write(v1alpha1.ApplicationPhaseFailed, msg)
	case msg != "" && mr == nil:
		return p.wait(msg)
	}
	desired, err := p.requesterDesired(mr)
	if err != nil {
		return p.fail(err)
	}
	if r.Suspend {
		return p.requesterSuspended(ctx, mr, desired)
	}
	if msg != "" {
		return p.wait(msg)
	}
	links := p.requesterLinks()
	if err := c.ApplyLinks(ctx, r, "root", links); err != nil {
		return ctlruntime.Result{}, err
	}
	if !c.Applied(r) {
		values := &manifests.Gateway{}
		digest, digestErr := compose.PhasesDigest(GatewaySet, values, nil, requesterSetPhase)
		if digestErr == nil && c.AppliedBefore(r, o.Status.AppliedSet, digest) {
			c.MarkApplied(r)
		} else {
			if _, err := p.write(v1alpha1.ApplicationPhaseProvisioning, "applying the node claim agent of its workspace"); err != nil {
				return ctlruntime.Result{}, err
			}
			if err := c.ApplySetPhases(ctx, r, GatewaySet, values, requesterSetPhase); err != nil {
				return p.retry("its workspace", err)
			}
			c.MarkApplied(r)
			if digestErr == nil {
				p.st.AppliedSet = &v1alpha1.AppliedSet{Generation: o.Generation, Digest: digest}
			}
		}
	}
	if ok, msg := c.LinksAccepted(ctx, r, links); !ok {
		return p.wait(msg)
	}
	if err := c.EnsureLinked(ctx, r, desired, desired); err != nil {
		return p.retry("", err)
	}
	if _, err := c.DeleteComponentsExcept(ctx, r.Workspace, r.Owner(), compose.DesiredKeys(desired)); err != nil {
		return ctlruntime.Result{}, err
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = mr.Status.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	if snap.Satisfied(desired) {
		p.st = compose.Ready(p.st, "the node market of the MarketRequester "+mr.Name+" is bound: its Requester rents the tenant clusters' nodes")
		return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.GatewayGVR, p.st)
	}
	return p.wait("waiting for the nodes-only GatewayAPI to be ready")
}

func (p *pass) requesterSuspended(ctx context.Context, mr *v1alpha1.MarketRequester, desired []compose.Desired) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	held := compose.Existing(snap, desired)
	if err := p.ensure(ctx, held); err != nil {
		return p.retry("", err)
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if mr != nil {
		p.st.Did = mr.Status.Did
	}
	compose.FillApplicationStatus(&p.st, o.Generation, true, o.Spec.Components, held, snap, nil)
	p.st = compose.SuspendedOnceApplied(p.st, snap, held)
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.GatewayGVR, p.st)
}

func (p *pass) deleteRequester(ctx context.Context) (ctlruntime.Result, error) {
	c, r := p.c, p.r
	if did := strings.TrimSpace(p.o.Status.Did); did != "" {
		clusters, err := c.DeleteClustersOf(ctx, did)
		if err != nil {
			return ctlruntime.Result{}, err
		}
		if clusters > 0 {
			return p.deleting(fmt.Sprintf("%d Cluster(s) of %s tearing down", clusters, did))
		}
	}
	left, err := c.StopComponents(ctx, r, "")
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if left == 0 {
		if err := c.DeleteLinks(ctx, r); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if left > 0 {
		return p.deleting(fmt.Sprintf("%d component(s) stopping", left))
	}
	c.Drop(r)
	return ctlruntime.Result{}, nil
}
