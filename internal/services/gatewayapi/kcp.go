package gatewayapi

import (
	"errors"
	"fmt"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clientcert"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	contractnodeclaim "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/nodeclaim"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
)

var (
	exposureGVR = exposurev1alpha1.ExposureGVR

	exposureServiceGVR = exposurev1alpha1.ServiceGVR
)

func gatewayServerFor(kcpClients component.KCPClients, nodeClaimAgent *kcpclient.ServiceAccountRef, workspace string, certs *clientcert.Issuer, publicHost, clusterStateDir string) (*gateway.Server, error) {
	server := &gateway.Server{}
	tenant, err := newTenantAPI(clusterStateDir)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the tenant API client: %w", err)
	}
	server.Tenant = tenant
	if kcpClients == nil {
		return server, nil
	}
	client, err := kcpClients.Client(workspace)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the gateway kcp client: %w", err)
	}
	restConfig := client.RESTConfig()
	if restConfig == nil || restConfig.Host == "" {
		return server, nil
	}
	agent := nodeClaimAgent
	if agent == nil {
		agent, _ = kcpclient.ParseServiceAccountRef(contractnodeclaim.AgentRef(workspace))
	}
	opts := kcpclient.Options{Host: restConfig.Host, RestConfig: restConfig, PublicHost: publicHost,
		HostProcessWorkspace: workspace, NodeClaimAgent: agent}
	if clusterStateDir != "" {
		opts.Tenant = ownerCredentials(clusterStateDir)
	}
	if certs != nil {
		opts.Kubeconfigs = certs
	}
	store, err := kcpclient.New(opts)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: the gateway kcp store: %w", err)
	}
	if certs != nil {
		certs.Recorder = store
	}
	server.Store = store
	server.Deny = store
	server.Revoker = store
	server.Feed = store
	return server, nil
}

func kcpStoreFor(kcpClients component.KCPClients, workspace string) (*kcpstore.Store, error) {
	if kcpClients == nil {
		return nil, errors.New("gatewayapi: no kcp client")
	}
	client, err := kcpClients.Client(workspace)
	if err != nil {
		return nil, fmt.Errorf("gatewayapi: kcp client for %s: %w", workspace, err)
	}
	restConfig := client.RESTConfig()
	if restConfig == nil || restConfig.Host == "" {
		return nil, fmt.Errorf("gatewayapi: the kcp client for %s has no rest config", workspace)
	}
	return kcpstore.New(kcpstore.Options{Host: restConfig.Host, RestConfig: restConfig})
}
