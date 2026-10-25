package repos

import (
	"context"
	"errors"
	"log/slog"

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
)

type Options struct {
	Bus *bus.Bus

	Log *slog.Logger

	Dynamic atprotorecord.DynamicFactory

	Network atprotorepo.RecordReader

	Pds pdsctl.Lookup

	AwaitPds pdsctl.Await

	Secrets func(ctx context.Context, workspace string, ref commonv1.SecretRef) (map[string]any, error)

	Scope func(workspace string) bool
}

func Register(m *ctlruntime.Manager, opts Options) error {
	pdsctl.Provide(pdsctl.Options{Bus: opts.Bus, Servers: opts.AwaitPds, Secrets: opts.Secrets})
	_, pdsErr := remotepds.Register(m, remotepds.Options{Log: opts.Log, Bus: opts.Bus})
	_, repoErr := atprotorepoctl.Register(m, atprotorepoctl.Options{Bus: opts.Bus, Log: opts.Log})
	_, localErr := localrepo.Register(m, localrepo.Options{Bus: opts.Bus, Network: opts.Network, Pds: opts.Pds, Scope: opts.Scope, Log: opts.Log})
	_, remoteErr := remoterepo.Register(m, remoterepo.Options{Bus: opts.Bus, Network: opts.Network, Log: opts.Log})
	_, recordErr := atprotorecord.Register(m, atprotorecord.Options{Bus: opts.Bus, Dynamic: opts.Dynamic, Scope: opts.Scope, Log: opts.Log})
	return errors.Join(pdsErr, repoErr, localErr, remoteErr, recordErr)
}
