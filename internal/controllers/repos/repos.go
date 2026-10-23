package repos

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecord"
	atprotorepoctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorepo"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/localrepo"
	pdsctl "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/pds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/remotepds"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/remoterepo"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pds/remote"
)

type Options struct {
	Bus *bus.Bus

	Log *slog.Logger

	Dynamic atprotorecord.DynamicFactory

	Network atprotorepo.RecordReader

	Pds pdsctl.Lookup

	Secrets func(ctx context.Context, workspace string, ref commonv1.SecretRef) (map[string]any, error)

	CABundle remotepds.CABundleFunc

	Identities remoterepo.IdentityReader

	Vault remoterepo.VaultStore

	HTTP *http.Client

	Scope func(workspace string) bool
}

func Register(m *ctlruntime.Manager, opts Options) error {
	pdsctl.Provide(pdsctl.Options{Bus: opts.Bus, Servers: opts.Pds, Secrets: opts.Secrets})
	remoterepo.ProvideCredentials(remoterepo.CredentialOptions{Bus: opts.Bus, Identities: opts.Identities, Vault: opts.Vault, HTTP: opts.HTTP})
	refs := func(workspace string) remote.Secrets {
		if opts.Secrets == nil {
			return nil
		}
		return func(ctx context.Context, ref commonv1.SecretRef) (map[string]any, error) {
			return opts.Secrets(ctx, workspace, ref)
		}
	}
	_, pdsErr := remotepds.Register(m, remotepds.Options{Log: opts.Log, Bus: opts.Bus, CABundle: opts.CABundle, SecretRefs: refs})
	_, repoErr := atprotorepoctl.Register(m, atprotorepoctl.Options{Bus: opts.Bus, Log: opts.Log})
	_, localErr := localrepo.Register(m, localrepo.Options{Bus: opts.Bus, Network: opts.Network, Pds: opts.Pds, Scope: opts.Scope, Log: opts.Log})
	_, remoteErr := remoterepo.Register(m, remoterepo.Options{Bus: opts.Bus, Network: opts.Network, Log: opts.Log})
	_, recordErr := atprotorecord.Register(m, atprotorecord.Options{Bus: opts.Bus, Dynamic: opts.Dynamic, Scope: opts.Scope, Log: opts.Log})
	return errors.Join(pdsErr, repoErr, localErr, remoteErr, recordErr)
}
