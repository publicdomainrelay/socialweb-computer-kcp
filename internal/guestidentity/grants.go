package guestidentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	DefaultResync = 30 * time.Second

	DefaultRetryDelay = 5 * time.Second

	DefaultMaxRetry = 6
)

type GrantStore interface {
	List(ctx context.Context) ([]v1alpha1.GuestIdentityGrant, error)

	Apply(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error

	Delete(ctx context.Context, name string) error
}

type GrantStoreFuncs struct {
	ListFunc func(context.Context) ([]v1alpha1.GuestIdentityGrant, error)

	ApplyFunc func(context.Context, v1alpha1.GuestIdentityGrant) error

	DeleteFunc func(context.Context, string) error
}

func (f GrantStoreFuncs) List(ctx context.Context) ([]v1alpha1.GuestIdentityGrant, error) {
	return f.ListFunc(ctx)
}

func (f GrantStoreFuncs) Apply(ctx context.Context, grant v1alpha1.GuestIdentityGrant) error {
	return f.ApplyFunc(ctx, grant)
}

func (f GrantStoreFuncs) Delete(ctx context.Context, name string) error {
	return f.DeleteFunc(ctx, name)
}

type RBACSource interface {
	Records(ctx context.Context, requesterDID string) ([]RBACRecord, error)
}

type RBACSourceFunc func(context.Context, string) ([]RBACRecord, error)

func (f RBACSourceFunc) Records(ctx context.Context, requesterDID string) ([]RBACRecord, error) {
	return f(ctx, requesterDID)
}

type GrantSubject struct {
	ComputeVM string

	OwnerUID string

	RequesterDID string

	Subject string
}

func GrantFor(subject GrantSubject, wish GrantWish) v1alpha1.GuestIdentityGrant {
	scopes := append([]string(nil), wish.Scopes...)
	claims := append([]v1alpha1.GuestIdentityClaim(nil), wish.Claims...)
	g := v1alpha1.GuestIdentityGrant{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "GuestIdentityGrant"},
		ObjectMeta: metav1.ObjectMeta{
			Name: GrantName(subject.ComputeVM, wish),
		},
		Spec: v1alpha1.GuestIdentityGrantSpec{
			ComputeVMRef: commonv1.LocalObjectRef{Name: subject.ComputeVM},
			Entity:       Entity(subject.ComputeVM),
			Api:          wish.API,
			Actx:         wish.Actx,
			Scopes:       scopes,
			Claims:       claims,
		},
	}
	if subject.OwnerUID != "" {
		block := true
		g.OwnerReferences = []metav1.OwnerReference{{
			APIVersion:         v1alpha1.GroupVersion.String(),
			Kind:               "ComputeVM",
			Name:               subject.ComputeVM,
			UID:                types.UID(subject.OwnerUID),
			BlockOwnerDeletion: &block,
		}}
	}
	return g
}

type SyncerOptions struct {
	Store GrantStore

	RBAC RBACSource

	Issuer func() string

	Log *slog.Logger

	Resync time.Duration

	RetryDelay time.Duration

	MaxRetry int
}

type Syncer struct {
	opts   SyncerOptions
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	guests map[string]*guestSync
	closed bool
}

type guestSync struct {
	subject GrantSubject
	ctx     context.Context
	cancel  context.CancelFunc
	kick    chan struct{}
}

func NewSyncer(ctx context.Context, opts SyncerOptions) (*Syncer, error) {
	if opts.Store == nil {
		return nil, errors.New("guestidentity: the grant syncer has no store")
	}
	if opts.RBAC == nil {
		return nil, errors.New("guestidentity: the grant syncer has no RBAC source")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Resync <= 0 {
		opts.Resync = DefaultResync
	}
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = DefaultRetryDelay
	}
	if opts.MaxRetry <= 0 {
		opts.MaxRetry = DefaultMaxRetry
	}
	root, cancel := context.WithCancel(ctx)
	return &Syncer{opts: opts, ctx: root, cancel: cancel, guests: map[string]*guestSync{}}, nil
}

func (s *Syncer) Register(ctx context.Context, subject GrantSubject) error {
	if err := checkSubject(subject); err != nil {
		return err
	}
	if err := s.syncOnce(ctx, subject); err != nil {
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("guestidentity: the grant syncer is closed")
	}
	if old, ok := s.guests[subject.ComputeVM]; ok {
		old.cancel()
	}
	stateCtx, cancel := context.WithCancel(s.ctx)
	state := &guestSync{subject: subject, ctx: stateCtx, cancel: cancel, kick: make(chan struct{}, 1)}
	s.guests[subject.ComputeVM] = state
	s.wg.Go(func() { s.run(state) })
	s.mu.Unlock()
	return nil
}

func (s *Syncer) Forget(computevm string) {
	s.mu.Lock()
	state, ok := s.guests[computevm]
	if ok {
		delete(s.guests, computevm)
	}
	s.mu.Unlock()
	if ok {
		state.cancel()
	}
}

func (s *Syncer) RequesterRBACChanged(requesterDID string) {
	s.mu.Lock()
	states := make([]*guestSync, 0, len(s.guests))
	for _, state := range s.guests {
		if state.subject.RequesterDID == requesterDID {
			states = append(states, state)
		}
	}
	s.mu.Unlock()
	for _, state := range states {
		select {
		case state.kick <- struct{}{}:
		default:
		}
	}
}

func (s *Syncer) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	states := make([]*guestSync, 0, len(s.guests))
	for _, state := range s.guests {
		states = append(states, state)
	}
	s.guests = map[string]*guestSync{}
	s.mu.Unlock()
	s.cancel()
	for _, state := range states {
		state.cancel()
	}
	s.wg.Wait()
}

func (s *Syncer) run(state *guestSync) {
	failures := 0
	for {
		delay := s.opts.Resync
		if failures > 0 {
			delay = s.opts.RetryDelay
			for i := 1; i < failures && i < s.opts.MaxRetry; i++ {
				delay *= 2
			}
			if delay > s.opts.Resync {
				delay = s.opts.Resync
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-state.ctx.Done():
			timer.Stop()
			return
		case <-state.kick:
			timer.Stop()
		case <-timer.C:
		}
		if state.ctx.Err() != nil {
			return
		}
		if err := s.syncOnce(state.ctx, state.subject); err != nil {
			if state.ctx.Err() != nil {
				return
			}
			failures++
			s.opts.Log.Warn("guest identity grants did not sync", "computevm", state.subject.ComputeVM, "err", err.Error())
			continue
		}
		failures = 0
	}
}

func (s *Syncer) syncOnce(ctx context.Context, subject GrantSubject) error {
	issuer := ""
	if s.opts.Issuer != nil {
		issuer = s.opts.Issuer()
	}
	records, readable, err := s.records(ctx, subject.RequesterDID)
	if err != nil {
		s.opts.Log.Warn("the requester's RBAC records are unreadable; the grants it already holds stay",
			"computevm", subject.ComputeVM, "requester", subject.RequesterDID, "err", err.Error())
	}
	wishes := Wishes(subject.ComputeVM, records, issuer, subject.Subject)
	held, err := s.opts.Store.List(ctx)
	if err != nil {
		return fmt.Errorf("listing the GuestIdentityGrants: %w", err)
	}
	current := map[string]v1alpha1.GuestIdentityGrant{}
	for _, g := range held {
		if g.Spec.ComputeVMRef.Name == subject.ComputeVM && g.DeletionTimestamp == nil {
			current[g.Name] = g
		}
	}
	wanted := map[string]bool{}
	for _, wish := range wishes {
		name := GrantName(subject.ComputeVM, wish)
		wanted[name] = true
		if g, ok := current[name]; ok && HeldKey(g) == WishKey(wish) {
			continue
		}
		if err := s.opts.Store.Apply(ctx, GrantFor(subject, wish)); err != nil {
			return fmt.Errorf("applying the grant for %s: %w", wish.Audience(), err)
		}
		s.opts.Log.Info("a guest identity grant is written", "computevm", subject.ComputeVM,
			"api", wish.API, "actx", wish.Actx, "scopes", wish.Scopes)
	}
	if !readable {
		return nil
	}
	for name := range current {
		if wanted[name] {
			continue
		}
		if err := s.opts.Store.Delete(ctx, name); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting the grant %s: %w", name, err)
		}
		s.opts.Log.Info("a guest identity grant is revoked", "computevm", subject.ComputeVM, "grant", name)
	}
	return nil
}

func (s *Syncer) records(ctx context.Context, requesterDID string) ([]RBACRecord, bool, error) {
	records, err := s.opts.RBAC.Records(ctx, requesterDID)
	if err != nil {
		return nil, false, err
	}
	return records, true, nil
}

func checkSubject(subject GrantSubject) error {
	switch {
	case !ValidComputeVM(subject.ComputeVM):
		return fmt.Errorf("guestidentity: %q is not a ComputeVM name", subject.ComputeVM)
	case subject.RequesterDID == "":
		return errors.New("guestidentity: the grant subject names no requester")
	case subject.Subject == "":
		return errors.New("guestidentity: the grant subject names no OIDC subject")
	}
	return nil
}
