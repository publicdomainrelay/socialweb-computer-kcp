package appctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/pdsclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/statusstate"
)

const AtprotoRepoFieldManager = "atprotorepo-controller"

type AtprotoRepoOptions struct {
	Resolver *pdsclient.RepoResolver

	Log *slog.Logger
}

type AtprotoRepoController struct {
	resolver *pdsclient.RepoResolver

	log *slog.Logger
}

func NewAtprotoRepoController(opts AtprotoRepoOptions) (*AtprotoRepoController, error) {
	if opts.Resolver == nil {
		return nil, errors.New("appctl: the AtprotoRepo controller needs a repo resolver")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &AtprotoRepoController{resolver: opts.Resolver, log: log.With("component", "atprotorepo-controller")}, nil
}

func (c *AtprotoRepoController) Reconcile(ctx context.Context, dyn dynamic.Interface, workspace string, u *unstructured.Unstructured) error {
	if u == nil || u.GetDeletionTimestamp() != nil {
		return nil
	}
	if dyn == nil {
		return errors.New("appctl: the AtprotoRepo controller needs a dynamic client")
	}
	var object v1alpha1.AtprotoRepo
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &object); err != nil {
		return fmt.Errorf("appctl: decoding AtprotoRepo %s: %w", u.GetName(), err)
	}
	generation := object.Generation
	status := object.Status
	status.ObservedGeneration = generation
	didText := strings.TrimSpace(object.Spec.DID)
	if didText == "" {
		setRepoCondition(&status.Conditions, generation, statusstate.ConditionReady, false, statusstate.ReasonWaiting,
			"the AtprotoRepo needs a did")
		setRepoInSync(&status.Conditions, generation, false, statusstate.ReasonWaiting, "the AtprotoRepo has no did")
		return c.writeStatus(ctx, dyn, workspace, u, status)
	}
	did, err := syntax.ParseDID(didText)
	if err != nil {
		setRepoCondition(&status.Conditions, generation, statusstate.ConditionReady, false, statusstate.ReasonSourceError,
			"the AtprotoRepo did is invalid: "+err.Error())
		setRepoInSync(&status.Conditions, generation, false, statusstate.ReasonSourceError, "the AtprotoRepo did is invalid")
		return c.writeStatus(ctx, dyn, workspace, u, status)
	}
	binding, err := repoBindingFor(workspace, object.Spec)
	if err != nil {
		setRepoCondition(&status.Conditions, generation, statusstate.ConditionReady, false, statusstate.ReasonWaiting,
			err.Error())
		setRepoInSync(&status.Conditions, generation, false, statusstate.ReasonWaiting, err.Error())
		return c.writeStatus(ctx, dyn, workspace, u, status)
	}
	resolved, err := c.resolver.Resolve(ctx, binding)
	if err != nil {
		reason := statusstate.ReasonSourceError
		if errors.Is(err, pdsclient.ErrAuthenticationRequired) || errors.Is(err, pdsclient.ErrInvalidConfig) {
			reason = statusstate.ReasonSourceUnavailable
		}
		setRepoCondition(&status.Conditions, generation, statusstate.ConditionReady, false, reason,
			"resolving the repo: "+err.Error())
		setRepoInSync(&status.Conditions, generation, false, reason, "the repo binding does not resolve")
		c.log.Warn("resolving an AtprotoRepo", "workspace", workspace, "name", object.Name, "err", err.Error())
		return c.writeStatus(ctx, dyn, workspace, u, status)
	}
	observation, err := statusstate.ObserveRepo(ctx, resolved.PDS, did)
	if err != nil {
		setRepoCondition(&status.Conditions, generation, statusstate.ConditionReady, false, statusstate.ReasonSourceError,
			"reading the repo: "+err.Error())
		setRepoInSync(&status.Conditions, generation, false, statusstate.ReasonSourceError, "the repo head is unreadable")
		return c.writeStatus(ctx, dyn, workspace, u, status)
	}
	status.Head = observation.Head
	status.Rev = observation.Rev
	status.Seq = observation.Seq
	status.KeyFingerprint = observation.KeyFingerprint
	message := "the repo head is " + observation.Head
	setRepoCondition(&status.Conditions, generation, statusstate.ConditionReady, true, statusstate.ReasonReady, message)
	setRepoInSync(&status.Conditions, generation, true, statusstate.ReasonReady, message)
	return c.writeStatus(ctx, dyn, workspace, u, status)
}

func repoBindingFor(workspace string, spec v1alpha1.AtprotoRepoSpec) (pdsclient.RepoBinding, error) {
	binding := pdsclient.RepoBinding{Workspace: workspace, DID: strings.TrimSpace(spec.DID)}
	if spec.PdsRef == nil {
		return pdsclient.RepoBinding{}, errors.New("the AtprotoRepo needs a pdsRef")
	}
	binding.Kind = strings.TrimSpace(spec.PdsRef.Kind)
	binding.Name = strings.TrimSpace(spec.PdsRef.Name)
	if binding.Workspace = strings.TrimSpace(spec.PdsRef.Workspace); binding.Workspace == "" {
		binding.Workspace = workspace
	}
	if binding.Name == "" {
		return pdsclient.RepoBinding{}, errors.New("the AtprotoRepo pdsRef has no name")
	}
	if spec.AuthRef != nil {
		binding.AuthRef = spec.AuthRef.DeepCopy()
	}
	return binding, nil
}

func (c *AtprotoRepoController) writeStatus(ctx context.Context, dyn dynamic.Interface, workspace string,
	u *unstructured.Unstructured, status v1alpha1.AtprotoRepoStatus) error {
	desired, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("appctl: encoding the status of %s: %w", u.GetName(), err)
	}
	ri := dyn.Resource(statusstate.AtprotoRepoGVR)
	name := u.GetName()
	var current *unstructured.Unstructured
	for range 5 {
		if current == nil {
			current, err = ri.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("appctl: reading AtprotoRepo %s: %w", name, err)
			}
		}
		if current.GetDeletionTimestamp() != nil {
			return nil
		}
		existing, _, err := unstructured.NestedMap(current.Object, "status")
		if err != nil {
			return err
		}
		if reflect.DeepEqual(existing, desired) {
			return nil
		}
		next := current.DeepCopy()
		next.Object["status"] = desired
		if _, err := ri.UpdateStatus(ctx, next, metav1.UpdateOptions{FieldManager: AtprotoRepoFieldManager}); err == nil {
			return nil
		} else if apierrors.IsNotFound(err) {
			return nil
		} else if !apierrors.IsConflict(err) {
			return fmt.Errorf("appctl: updating the status of %s: %w", name, err)
		}
		current = nil
	}
	return fmt.Errorf("appctl: the status of %s stayed in conflict", name)
}

func setRepoCondition(conditions *[]metav1.Condition, generation int64, conditionType string, ok bool, reason, message string) {
	value := metav1.ConditionFalse
	if ok {
		value = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conditions, metav1.Condition{Type: conditionType, Status: value, Reason: reason,
		Message: message, ObservedGeneration: generation})
}

func setRepoInSync(conditions *[]metav1.Condition, generation int64, ok bool, reason, message string) {
	setRepoCondition(conditions, generation, statusstate.ConditionInSync, ok, reason, message)
}
