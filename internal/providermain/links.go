package providermain

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterprovider"
)

type clusterLinkClient interface {
	WriteClusterLink(ctx context.Context, logicalCluster string, link v1alpha1.ClusterLink) error
	WriteClusterLinkStatus(ctx context.Context, logicalCluster, name string, status v1alpha1.ClusterLinkStatus) (bool, error)
	DeleteClusterLink(ctx context.Context, logicalCluster, name string) error
}

type linkRecorder struct {
	client   clusterLinkClient
	registry clusterprovider.Clusters
}

var _ clusterprovider.LinkRecorder = linkRecorder{}

func (r linkRecorder) home(ctx context.Context, cluster string) (string, error) {
	ref, err := nodeRefFor(ctx, r.registry, cluster)
	if err != nil {
		return "", err
	}
	if ref.Home == "" {
		return "", fmt.Errorf("clusterprovider: the Cluster CR for %s was found with no home workspace, "+
			"so its ClusterLinks have nowhere to be written", cluster)
	}
	return ref.Home, nil
}

func (r linkRecorder) WriteLink(ctx context.Context, link v1alpha1.ClusterLink) error {
	home, err := r.home(ctx, link.Spec.ClusterRef.Name)
	if err != nil {
		return err
	}
	return r.client.WriteClusterLink(ctx, home, link)
}

func (r linkRecorder) WriteLinkStatus(ctx context.Context, cluster, name string, status v1alpha1.ClusterLinkStatus) error {
	home, err := r.home(ctx, cluster)
	if err != nil {
		return err
	}
	_, err = r.client.WriteClusterLinkStatus(ctx, home, name, status)
	return err
}

func (r linkRecorder) remove(ctx context.Context, home, cluster string) error {
	var errs []error
	for _, service := range clusterlink.Services {
		if err := r.client.DeleteClusterLink(ctx, home, clusterlink.Name(cluster, service)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func clusterLinks(cfg Config, client clusterLinkClient, registry clusterprovider.Clusters, log *slog.Logger) *clusterprovider.Links {
	if cfg.Dumbpipe == "" || cfg.ObserveOnly() || client == nil {
		return nil
	}
	return &clusterprovider.Links{
		Dumbpipe: cfg.Dumbpipe,
		Recorder: linkRecorder{client: client, registry: registry},
		Log:      log,
	}
}

func (p *Provider) removeLinks(ctx context.Context, ref clusterprovider.Ref, key string) {
	if p.links == nil {
		return
	}
	if err := p.links.remove(ctx, ref.Home, key); err != nil {
		p.log.Warn("cluster provider: removing the ClusterLinks of a deleted cluster did not complete",
			"cluster", ref.Name, "key", key, "home", ref.Home, "error", err.Error(),
			"keys", "left in the cluster's own vault at "+clusterlink.Mount+", which goes with its data")
		return
	}
	p.log.Info("cluster provider: removed the ClusterLinks of a deleted cluster",
		"cluster", ref.Name, "key", key, "home", ref.Home)
}
