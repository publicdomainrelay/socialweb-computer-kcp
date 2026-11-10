package atircnode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	Name = "atircnode"

	Export = "hostprocesses"

	Finalizer = "socialweb.computer/atircnode"

	LabelAtircNode = "socialweb.computer/atircnode"

	ChildPrefix = "atirc-"

	Asset = "atirc"

	ConditionReady = "Ready"
)

var (
	labelRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	handleRE  = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	channelRE = regexp.MustCompile(`^#[^\s,\x00]{1,63}$`)
)

type Options struct {
	Log *slog.Logger

	Prefix string

	StateDir string

	Scope func(workspace string) bool
}

type Controller struct {
	log *slog.Logger

	opts Options
}

func Register(m *ctlruntime.Manager, opts Options) (*Controller, error) {
	if opts.StateDir == "" {
		return nil, fmt.Errorf("atircnode: a state directory is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Controller{log: log.With("component", "atircnode-controller"), opts: opts}
	options := []ctlruntime.Option{ctlruntime.Export(Export), ctlruntime.Finalizer(Finalizer), ctlruntime.Owns[*v1alpha1.HostProcess]()}
	if opts.Scope != nil {
		options = append(options, ctlruntime.InWorkspaces(opts.Scope))
	}
	if err := ctlruntime.For[*v1alpha1.AtircNode](m, Name, c, options...); err != nil {
		return nil, err
	}
	return c, nil
}

func Identity(spec v1alpha1.AtircNodeSpec) string {
	handle := strings.ToLower(strings.TrimSpace(spec.Handle))
	if nick := strings.ToLower(strings.TrimSpace(spec.Nick)); nick != "" {
		return nick + "." + handle
	}
	return handle
}

func (c *Controller) configDir(o *v1alpha1.AtircNode) string {
	if o.Spec.ConfigDir != "" {
		return o.Spec.ConfigDir
	}
	return filepath.Join(c.opts.StateDir, "atirc", o.Name)
}

func validate(o *v1alpha1.AtircNode) error {
	s := o.Spec
	switch {
	case !labelRE.MatchString(o.Name) || len(o.Name) > 40:
		return fmt.Errorf("the name %q is not a DNS label of at most 40 characters", o.Name)
	case !handleRE.MatchString(strings.ToLower(strings.TrimSpace(s.Handle))):
		return fmt.Errorf("spec.handle %q is not a handle", s.Handle)
	case s.Nick != "" && !labelRE.MatchString(strings.ToLower(strings.TrimSpace(s.Nick))):
		return fmt.Errorf("spec.nick %q is not a DNS label", s.Nick)
	case s.ConfigDir != "" && !filepath.IsAbs(s.ConfigDir):
		return fmt.Errorf("spec.configDir %q is not an absolute path", s.ConfigDir)
	case s.PasswordEnvFile != "" && !filepath.IsAbs(s.PasswordEnvFile):
		return fmt.Errorf("spec.passwordEnvFile %q is not an absolute path", s.PasswordEnvFile)
	case s.IrcPort != nil && (*s.IrcPort < 0 || *s.IrcPort > 65535):
		return fmt.Errorf("spec.ircPort %d is not a port", *s.IrcPort)
	case s.Jetstream != "" && s.Jetstream != "off" && !strings.HasPrefix(s.Jetstream, "wss://") && !strings.HasPrefix(s.Jetstream, "ws://"):
		return fmt.Errorf("spec.jetstream %q is not a ws(s) URL or off", s.Jetstream)
	}
	for _, ch := range s.Channels {
		if !channelRE.MatchString(ch) {
			return fmt.Errorf("spec.channels: %q is not a channel", ch)
		}
	}
	for _, p := range s.Peers {
		if !handleRE.MatchString(strings.ToLower(strings.TrimSpace(p))) {
			return fmt.Errorf("spec.peers: %q is not a mesh identity", p)
		}
	}
	return nil
}

func (c *Controller) childSpec(o *v1alpha1.AtircNode) v1alpha1.HostProcessSpec {
	s := o.Spec
	argv := []string{"mesh", "run"}
	for _, ch := range s.Channels {
		argv = append(argv, "-c", ch)
	}
	for _, p := range s.Peers {
		argv = append(argv, "--peer", strings.ToLower(strings.TrimSpace(p)))
	}
	if s.Jetstream != "" {
		argv = append(argv, "--jetstream", s.Jetstream)
	}
	if s.IrcPort != nil {
		argv = append(argv, "--irc-port", strconv.Itoa(int(*s.IrcPort)))
	}
	if s.NoPublish {
		argv = append(argv, "--no-publish")
	}
	env := []v1alpha1.HostProcessEnv{{Name: "ATP_USERNAME", Value: strings.ToLower(strings.TrimSpace(s.Handle))},
		{Name: "ATIRC_CONFIG_DIR", Value: c.configDir(o)}}
	if s.Nick != "" {
		env = append(env, v1alpha1.HostProcessEnv{Name: "ATIRC_NICK", Value: strings.ToLower(strings.TrimSpace(s.Nick))})
	}
	spec := v1alpha1.HostProcessSpec{Unit: c.opts.Prefix + ChildPrefix + o.Name + ".service", Description: "atirc mesh run as " + Identity(s),
		Asset: &v1alpha1.HostProcessAsset{Name: Asset}, Argv: argv, Env: env, RestartPolicy: v1alpha1.HostProcessRestartAlways,
		Stop: &v1alpha1.HostProcessStop{Signal: "SIGTERM", GraceSeconds: 10}, Placement: v1alpha1.HostProcessPlacementSupervisor}
	if s.PasswordEnvFile != "" {
		spec.EnvFiles = []string{s.PasswordEnvFile}
	}
	key, _ := json.Marshal([]any{spec.Argv, spec.Env, spec.EnvFiles})
	sum := sha256.Sum256(key)
	spec.RestartKey = hex.EncodeToString(sum[:8])
	return spec
}

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.AtircNode) (ctlruntime.Result, error) {
	if !o.DeletionTimestamp.IsZero() {
		return c.finalize(ctx, req, o)
	}
	next := o.Status.DeepCopy()
	next.ObservedGeneration = o.Generation
	set := func(phase v1alpha1.AtircNodePhase, msg string) {
		next.Phase, next.Message = phase, msg
		conds.Set(&next.Conditions, o.Generation, ConditionReady, phase == v1alpha1.AtircNodePhaseRunning, string(phase), msg)
	}
	if err := validate(o); err != nil {
		set(v1alpha1.AtircNodePhaseFailed, err.Error())
		return ctlruntime.Result{}, c.apply(ctx, req, o, next)
	}
	next.Identity, next.ConfigDir = Identity(o.Spec), c.configDir(o)
	hp, err := c.child(ctx, req, o, c.childSpec(o))
	if err != nil {
		return ctlruntime.Result{}, err
	}
	next.HostProcess, next.Runtime = hp.Name, hp.Status.Runtime
	switch hp.Status.Phase {
	case v1alpha1.HostProcessPhaseRunning:
		set(v1alpha1.AtircNodePhaseRunning, "atirc mesh run is running as "+next.Identity)
	case v1alpha1.HostProcessPhaseLost:
		c.log.Warn("atircnode_hostprocess_lost", "workspace", req.Workspace, "atircnode", o.Name, "reason", string(hp.Status.LostReason))
		if err := req.Delete(ctx, hp); err != nil {
			return ctlruntime.Result{}, err
		}
		set(v1alpha1.AtircNodePhaseStarting, "its HostProcess "+hp.Name+" is Lost "+string(hp.Status.LostReason)+"; placing a new one")
	case v1alpha1.HostProcessPhaseFailed:
		set(v1alpha1.AtircNodePhaseFailed, "its HostProcess "+hp.Name+" is Failed: "+hp.Status.Message)
	case "":
		set(v1alpha1.AtircNodePhaseStarting, "its HostProcess "+hp.Name+" waits for the supervisor")
	default:
		set(v1alpha1.AtircNodePhaseStarting, "its HostProcess "+hp.Name+" is "+string(hp.Status.Phase))
	}
	return ctlruntime.Result{}, c.apply(ctx, req, o, next)
}

func (c *Controller) child(ctx context.Context, req ctlruntime.Request, o *v1alpha1.AtircNode, spec v1alpha1.HostProcessSpec) (*v1alpha1.HostProcess, error) {
	name := ChildPrefix + o.Name
	var cur v1alpha1.HostProcess
	err := req.Get(ctx, "", name, &cur)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && reflect.DeepEqual(cur.Spec, spec) {
		return &cur, nil
	}
	controller := true
	want := &v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelAtircNode: o.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "AtircNode", Name: o.Name,
			UID: o.UID, Controller: &controller}}}, Spec: spec}
	if err := req.Apply(ctx, want); err != nil {
		return nil, err
	}
	c.log.Info("atircnode_hostprocess_applied", "workspace", req.Workspace, "atircnode", o.Name, "hostprocess", name, "unit", spec.Unit)
	if err == nil && cur.Name != "" {
		cur.Spec = spec
		return &cur, nil
	}
	return want, nil
}

func (c *Controller) finalize(ctx context.Context, req ctlruntime.Request, o *v1alpha1.AtircNode) (ctlruntime.Result, error) {
	var hp v1alpha1.HostProcess
	err := req.Get(ctx, "", ChildPrefix+o.Name, &hp)
	switch {
	case apierrors.IsNotFound(err):
		return ctlruntime.Result{}, nil
	case err != nil:
		return ctlruntime.Result{Waiting: true}, err
	}
	if hp.DeletionTimestamp.IsZero() {
		if err := req.Delete(ctx, &hp); err != nil {
			return ctlruntime.Result{Waiting: true}, err
		}
	}
	return ctlruntime.Result{Waiting: true}, nil
}

func (c *Controller) apply(ctx context.Context, req ctlruntime.Request, o *v1alpha1.AtircNode, next *v1alpha1.AtircNodeStatus) error {
	if reflect.DeepEqual(o.Status, *next) {
		return nil
	}
	obj := o.DeepCopy()
	obj.Status = *next
	return req.ApplyStatus(ctx, obj)
}
