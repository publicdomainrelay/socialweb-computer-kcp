package remote

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

type NetworkConfig struct {
	Directory identity.Directory

	HTTP *http.Client
}

type Network struct {
	directory identity.Directory

	http *http.Client
}

var _ atprotorepo.RecordReader = (*Network)(nil)

func NewNetwork(cfg NetworkConfig) *Network {
	return &Network{directory: cfg.Directory, http: cfg.HTTP}
}

func (n *Network) repo(ctx context.Context, raw string) (*Repo, error) {
	if n.directory == nil {
		return nil, errors.New("remote: the network reader has no identity directory")
	}
	did, err := syntax.ParseDID(raw)
	if err != nil {
		return nil, fmt.Errorf("remote: %q is not a DID: %w", raw, err)
	}
	ident, err := n.directory.LookupDID(ctx, did)
	if err != nil {
		return nil, fmt.Errorf("remote: resolving %s: %w", did, err)
	}
	endpoint := ident.PDSEndpoint()
	if endpoint == "" {
		return nil, fmt.Errorf("remote: the DID document of %s names no PDS", did)
	}
	client, err := pdsclient.NewRemote(pdsclient.RemoteConfig{BaseURL: endpoint, HTTPClient: n.http, Timeout: DefaultTimeout})
	if err != nil {
		return nil, err
	}
	return Wrap(client, did), nil
}

func (n *Network) GetRecord(ctx context.Context, repo, collection, rkey string) (atprotorepo.Record, error) {
	r, err := n.repo(ctx, repo)
	if err != nil {
		return atprotorepo.Record{}, err
	}
	return r.GetRecord(ctx, repo, collection, rkey)
}

func (n *Network) ListRecords(ctx context.Context, repo, collection string, opts atprotorepo.ListOptions) (atprotorepo.ListPage, error) {
	r, err := n.repo(ctx, repo)
	if err != nil {
		return atprotorepo.ListPage{}, err
	}
	return r.ListRecords(ctx, repo, collection, opts)
}

func (n *Network) Resolve(ctx context.Context, ref commonv1.StrongRef) (atprotorepo.Record, error) {
	uri, err := syntax.ParseATURI(ref.URI)
	if err != nil {
		return atprotorepo.Record{}, fmt.Errorf("remote: the record uri %q: %w", ref.URI, err)
	}
	r, err := n.repo(ctx, uri.Authority().String())
	if err != nil {
		return atprotorepo.Record{}, err
	}
	return r.Resolve(ctx, ref)
}
