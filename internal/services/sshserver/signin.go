package sshserver

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/policy"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gateway/custody"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/gatewayhttp"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/supervise"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/runs"
)

const (
	VaultClient = "ssh-gateway"

	SignInWorkflow = "bidder-tangled-vouch"

	atprotoMount = "atproto"

	renewBelow = 30 * time.Minute

	renewIncrement = time.Hour

	signInPolicyTimeout = 30 * time.Second
)

var errNoOperator = errors.New("this ssh gateway names no operator, so its policy admits no account")

type webSignIn struct {
	endpoint custody.Endpoint

	handler http.Handler

	signIn *custody.SignIn

	auth *gatewayhttp.RequestAuth

	oauth *gatewayhttp.OAuth

	tasks *supervise.Group
}

func (w *webSignIn) close() {
	if w == nil {
		return
	}
	_ = w.tasks.Stop()
	w.signIn.Close()
}

func (s *Service) publicOrigin() string {
	return strings.TrimRight(strings.TrimSpace(s.spec.PublicOrigin), "/")
}

func webSignInMode(spec v1alpha1.SshServerSpec, ownerSessions int) bool {
	return spec.VaultRef != nil && strings.TrimSpace(spec.PublicOrigin) != "" && ownerSessions == 0
}

func (s *Service) openSignIn(ctx context.Context) (*webSignIn, error) {
	if !webSignInMode(s.spec, len(s.ownerSessions)) {
		return nil, nil
	}
	origin := s.publicOrigin()
	endpoint, err := s.vaultEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	signIn, err := custody.New(ctx, custody.Config{Endpoint: endpoint, Role: custody.DefaultRole, RenewBelow: renewBelow,
		RenewIncrement: renewIncrement, Network: s.spec.Network}, custody.Deps{TLS: s.tls, Log: s.log})
	if err != nil {
		return nil, err
	}
	tasks := supervise.New(ctx)
	web := &webSignIn{endpoint: endpoint, signIn: signIn, tasks: tasks}
	if web.auth, err = gatewayhttp.NewRequestAuth(gatewayhttp.RequestAuthOptions{SignIn: signIn, PublicOrigin: origin, Log: s.log}); err != nil {
		web.close()
		return nil, err
	}
	if web.oauth, err = gatewayhttp.NewOAuth(gatewayhttp.OAuthOptions{SignIn: signIn, Auth: web.auth, PublicOrigin: origin,
		PostSignInURL: origin + "/", OnSignedIn: s.signedIn, OnSignedOut: s.signedOut, Tasks: tasks, Log: s.log}); err != nil {
		web.close()
		return nil, err
	}
	web.handler = web.oauth.Handler()
	return web, nil
}

func (s *Service) vaultEndpoint(ctx context.Context) (custody.Endpoint, error) {
	vref := s.spec.VaultRef
	if s.deps.KCP == nil {
		return custody.Endpoint{}, errors.New("sshserver: no kcp client for the vault")
	}
	workspace := cmp.Or(vref.Workspace, s.workspace())
	client, err := s.deps.KCP.Client(workspace)
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: kcp client for %s: %w", workspace, err)
	}
	restConfig := client.RESTConfig()
	if restConfig == nil || restConfig.Host == "" {
		return custody.Endpoint{}, fmt.Errorf("sshserver: the kcp client for %s has no rest config", workspace)
	}
	store, err := kcpstore.New(kcpstore.Options{Host: restConfig.Host, RestConfig: restConfig})
	if err != nil {
		return custody.Endpoint{}, err
	}
	server, err := kcpstore.OfClusterScoped[v1alpha1.OpenBaoServer](store, v1alpha1.OpenBaoServerGVR).Get(ctx, ref.New(workspace, "", vref.Name))
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: reading OpenBaoServer %s/%s: %w", workspace, vref.Name, err)
	}
	endpoint, _, err := custody.EndpointFor(server.Status, VaultClient, atprotoMount)
	if err != nil {
		return custody.Endpoint{}, fmt.Errorf("sshserver: OpenBaoServer %s/%s: %w", workspace, vref.Name, err)
	}
	return endpoint, nil
}

func (s *Service) self() commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: Kind, Name: s.deps.Self.Name, Workspace: s.workspace()}
}

func (s *Service) sessionOwners(ctx context.Context) (contractmarket.SessionOwnerIntents, error) {
	if s.deps.Bus == nil {
		return nil, errors.New("sshserver: no bus")
	}
	self := s.self()
	return bus.Resolve[contractmarket.SessionOwnerIntents](ctx, s.deps.Bus, self, self, bus.Capability{Name: contractmarket.CapabilitySessionOwners})
}

func (s *Service) policyRuns() *runs.Client {
	handlers := s.deps.Bus
	self := s.self()
	return &runs.Client{
		Resolve: func(ctx context.Context, engine commonv1.ObjectRef, workflow string) (policy.Runner, error) {
			return bus.Resolve[policy.Runner](ctx, handlers, self, engine, bus.Capability{Name: policy.CapabilityRun, Scope: []string{workflow}})
		},
		Owner:   policy.Owner{APIVersion: v1alpha1.GroupVersion.String(), Kind: Kind, Name: s.deps.Self.Name, UID: string(s.deps.UID)},
		Engine:  runs.EngineRef(s.spec.PolicyEngineRef, s.workspace()),
		Role:    "sshserver",
		Prefix:  "sshserver-policy",
		Timeout: signInPolicyTimeout,
		Parser:  runs.Strict,
		Now:     time.Now,
	}
}

func (s *Service) admit(ctx context.Context, did string) error {
	operator := strings.TrimSpace(s.spec.OperatorDid)
	if operator == "" {
		return errNoOperator
	}
	verdict := s.policyRuns().Run(ctx, runs.Request{WorkflowRef: SignInWorkflow, Input: runs.Input{Perspective: "bidder", Mode: runs.ModeScope,
		SelfDid: operator, SubjectDid: did, CounterpartyDid: did}})
	if !verdict.Allow {
		return fmt.Errorf("the %s policy of %s does not admit %s: %s", SignInWorkflow, operator, did, verdict.Reason)
	}
	return nil
}

func (s *Service) signedIn(ctx context.Context, user *gatewayhttp.WebUser) error {
	if user == nil || strings.TrimSpace(user.DID) == "" {
		return errors.New("sshserver: the sign-in names no DID")
	}
	if err := s.admit(ctx, user.DID); err != nil {
		s.log.Info("sshserver_sign_in_refused", "did", user.DID, "reason", err.Error())
		return err
	}
	owners, err := s.sessionOwners(ctx)
	if err != nil {
		return fmt.Errorf("sshserver: the session owners handle: %w", err)
	}
	if err := owners.EnsureSessionOwner(ctx, user.DID); err != nil {
		s.log.Warn("sshserver_sign_in_unrecorded", "did", user.DID, "err", err.Error())
		return err
	}
	s.log.Info("sshserver_account_signed_in", "did", user.DID)
	return nil
}

func (s *Service) signedOut(ctx context.Context, user *gatewayhttp.WebUser) error {
	if user == nil || strings.TrimSpace(user.DID) == "" {
		return nil
	}
	owners, err := s.sessionOwners(ctx)
	if err != nil {
		return fmt.Errorf("sshserver: the session owners handle: %w", err)
	}
	if err := owners.DeleteSessionOwner(ctx, user.DID); err != nil {
		return err
	}
	s.log.Info("sshserver_account_signed_out", "did", user.DID)
	return nil
}

func (s *Service) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(signInPage))
}
