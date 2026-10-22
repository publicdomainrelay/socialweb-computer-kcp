package statusstate

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
)

func (c *Controller) reconcileRepo(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.AtprotoRepo
	if err := decode(u, &obj); err != nil {
		c.log.Warn("decoding an AtprotoRepo", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	didText := strings.TrimSpace(obj.Spec.DID)
	ref := strings.TrimSpace(obj.Spec.PdsRef)
	if didText == "" || ref == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the AtprotoRepo needs a did and a pdsRef")
		c.write(ctx, dyn, AtprotoRepoGVR, u, &status)
		return
	}
	did, err := syntax.ParseDID(didText)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "the AtprotoRepo did is invalid: "+err.Error())
		c.write(ctx, dyn, AtprotoRepoGVR, u, &status)
		return
	}
	if c.pds == nil {
		setReady(&status.Conditions, generation, false, ReasonSourceUnavailable, "no PDS resolver is configured")
		c.write(ctx, dyn, AtprotoRepoGVR, u, &status)
		return
	}
	pds, err := c.pds.ResolvePDS(ctx, ref)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "resolving the pdsRef: "+err.Error())
		c.write(ctx, dyn, AtprotoRepoGVR, u, &status)
		return
	}
	observation, err := observeRepo(ctx, pds, did)
	if err != nil {
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the repo: "+err.Error())
		c.write(ctx, dyn, AtprotoRepoGVR, u, &status)
		return
	}
	status.Head = observation.Head
	status.Rev = observation.Rev
	status.Seq = observation.Seq
	status.KeyFingerprint = observation.KeyFingerprint
	setReady(&status.Conditions, generation, true, ReasonReady, "the repo head is "+observation.Head)
	c.write(ctx, dyn, AtprotoRepoGVR, u, &status)
}

func observeRepo(ctx context.Context, pds pdsclient.PDS, did syntax.DID) (RepoObservation, error) {
	head, rev, err := pds.GetLatestCommit(ctx, did)
	if err == nil {
		out := RepoObservation{Head: head.String(), Rev: rev.String()}
		fillRepoDetails(ctx, pds, did, &out)
		return out, nil
	}
	body, repoErr := pds.GetRepo(ctx, did, nil)
	if repoErr != nil {
		return RepoObservation{}, fmt.Errorf("%w; reading the repo CAR: %v", err, repoErr)
	}
	out, carErr := repoObservationFromCAR(ctx, body, did)
	if carErr != nil {
		return RepoObservation{}, carErr
	}
	fillRepoDetails(ctx, pds, did, &out)
	return out, nil
}

func repoObservationFromCAR(ctx context.Context, body io.ReadCloser, did syntax.DID) (RepoObservation, error) {
	defer body.Close()
	commit, root, err := repo.LoadCommitFromCAR(ctx, body)
	if err != nil {
		return RepoObservation{}, fmt.Errorf("the repo CAR: %w", err)
	}
	if commit.DID != did.String() {
		return RepoObservation{}, fmt.Errorf("the repo CAR commit is for %q, not %q", commit.DID, did.String())
	}
	return RepoObservation{Head: root.String(), Rev: commit.Rev}, nil
}

func fillRepoDetails(ctx context.Context, pds pdsclient.PDS, did syntax.DID, out *RepoObservation) {
	source, ok := pds.(RepoDetailsSource)
	if !ok {
		return
	}
	details, err := source.RepoDetails(ctx, did)
	if err != nil {
		return
	}
	out.Seq = details.Seq
	out.KeyFingerprint = details.KeyFingerprint
}
