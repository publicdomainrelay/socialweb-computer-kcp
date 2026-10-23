package atprotorecordwatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atprotorepo/binding"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/atprotorecord"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/eventstream"
)

type transientError struct {
	err error
}

func (e transientError) Error() string { return e.err.Error() }

func (e transientError) Unwrap() error { return e.err }

func readFailed(err error) error {
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return err
	}
	return transientError{err: err}
}

func (c *Controller) repo(ctx context.Context, workspace string, spec v1alpha1.AtprotoRecordWatchSpec) (eventstream.WatchRepo, []string, error) {
	if spec.RepoRef == nil {
		did, err := eventstream.SpecRepo(spec)
		return eventstream.WatchRepo{DID: did}, nil, err
	}
	ref := *spec.RepoRef
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return eventstream.WatchRepo{}, nil, errors.New("spec.repoRef needs a name")
	}
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), RepoKind) {
		return eventstream.WatchRepo{}, nil, fmt.Errorf("spec.repoRef must name an %s, not %q", RepoKind, ref.Kind)
	}
	repoWorkspace := cmp.Or(strings.TrimSpace(ref.Workspace), workspace)
	keys := []string{binding.Key(repoWorkspace, RepoKind, name)}
	if c.repos == nil {
		return eventstream.WatchRepo{}, keys, errors.New("no reader for AtprotoRepo objects is configured")
	}
	repo, err := c.repos.AtprotoRepo(ctx, repoWorkspace, name)
	if err != nil {
		return eventstream.WatchRepo{}, keys, readFailed(fmt.Errorf("reading AtprotoRepo %s: %w", name, err))
	}
	out := eventstream.WatchRepo{DID: strings.TrimSpace(repo.Status.DID),
		Ref: &commonv1.ObjectRef{Workspace: repoWorkspace, Group: v1alpha1.GroupName, Kind: RepoKind, Name: name}}
	backend := repo.Spec.BackendRef
	backendName := strings.TrimSpace(backend.Name)
	if backendName == "" {
		if out.DID != "" {
			return out, keys, nil
		}
		return eventstream.WatchRepo{}, keys, fmt.Errorf("AtprotoRepo %s names no backendRef", name)
	}
	backendWorkspace := backend.InWorkspace(repoWorkspace)
	kind := strings.TrimSpace(backend.Kind)
	keys = append(keys, binding.Key(backendWorkspace, kind, backendName))
	if out.DID != "" {
		return out, keys, nil
	}
	switch kind {
	case LocalRepoKind:
		local, err := c.repos.LocalRepo(ctx, backendWorkspace, backendName)
		if err != nil {
			return eventstream.WatchRepo{}, keys, readFailed(fmt.Errorf("reading LocalRepo %s: %w", backendName, err))
		}
		out.DID = strings.TrimSpace(local.Spec.DID)
	case RemoteRepoKind:
		remote, err := c.repos.RemoteRepo(ctx, backendWorkspace, backendName)
		if err != nil {
			return eventstream.WatchRepo{}, keys, readFailed(fmt.Errorf("reading RemoteRepo %s: %w", backendName, err))
		}
		out.DID = strings.TrimSpace(remote.Spec.DID)
	default:
		return eventstream.WatchRepo{}, keys, fmt.Errorf("AtprotoRepo %s backendRef kind %q is neither LocalRepo nor RemoteRepo", name, backend.Kind)
	}
	return out, keys, nil
}

func matchKeys(workspace string, spec v1alpha1.AtprotoRecordWatchSpec) []string {
	var out []string
	for _, match := range spec.Match {
		if match.ValueFrom != nil && strings.TrimSpace(match.ValueFrom.Name) != "" {
			out = append(out, atprotorecord.ReferenceKey(workspace, *match.ValueFrom))
		}
	}
	return out
}
