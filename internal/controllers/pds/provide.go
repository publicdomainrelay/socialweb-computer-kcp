package pds

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	atpds "github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	pdscontract "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/local"
)

const Kind = "Pds"

var GroupKind = schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind}

type Lookup func(workspace, name string) (*atpds.Server, bool)

type Secrets func(ctx context.Context, workspace string, ref commonv1.SecretRef) (map[string]any, error)

type Options struct {
	Bus *bus.Bus

	Servers Lookup

	Secrets Secrets
}

func Provide(opts Options) {
	server := func(to commonv1.ObjectRef) (*atpds.Server, error) {
		if opts.Servers == nil {
			return nil, fmt.Errorf("%w: no in-process Pds lookup is configured", pdscontract.ErrUnsupported)
		}
		srv, ok := opts.Servers(strings.TrimSpace(to.Workspace), strings.TrimSpace(to.Name))
		if !ok || srv == nil {
			return nil, fmt.Errorf("%w: the Pds %s is not running in this process", pdscontract.ErrNotFound, to.String())
		}
		return srv, nil
	}
	secrets := func(workspace string) local.Secrets {
		return func(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error) {
			if opts.Secrets == nil {
				return nil, fmt.Errorf("%w: no secret resolver is configured", pdscontract.ErrUnsupported)
			}
			return opts.Secrets(ctx, workspace, ref)
		}
	}
	bus.Provide(opts.Bus, GroupKind, pdscontract.CapabilityAccount, func(ctx context.Context, to commonv1.ObjectRef, scope []string) (atprotorepo.Repo, error) {
		srv, err := server(to)
		if err != nil {
			return nil, err
		}
		return local.NewRepo(ctx, srv, scope)
	})
	bus.Provide(opts.Bus, GroupKind, pdscontract.CapabilityAccount, func(_ context.Context, to commonv1.ObjectRef, scope []string) (pdscontract.Accounts, error) {
		srv, err := server(to)
		if err != nil {
			return nil, err
		}
		return local.NewAccounts(srv, secrets(to.Workspace), scope), nil
	})
	bus.Provide(opts.Bus, GroupKind, pdscontract.CapabilitySession, func(_ context.Context, to commonv1.ObjectRef, scope []string) (pdscontract.Sessions, error) {
		srv, err := server(to)
		if err != nil {
			return nil, err
		}
		return local.NewSessions(srv, secrets(to.Workspace), scope), nil
	})
	bus.Provide(opts.Bus, GroupKind, pdscontract.CapabilitySync, func(_ context.Context, to commonv1.ObjectRef, _ []string) (pdscontract.Sync, error) {
		srv, err := server(to)
		if err != nil {
			return nil, err
		}
		return local.NewSync(srv), nil
	})
}
