package childapplication

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type observed struct {
	workspace string

	workspaceExists bool

	workspaceReady bool

	workspaceDeleting bool

	coreExpected bool

	coreFound bool

	coreRef string

	coreReady bool

	coreFailed bool

	components []v1alpha1.ChildApplicationComponent

	componentsReady bool

	componentFailed bool

	stopped bool

	deleting bool

	invalid string

	orphaned bool

	orphanReason string

	orphanMessage string
}

func (c *Controller) Reconcile(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) error {
	if dyn == nil || u == nil {
		return nil
	}
	var ca v1alpha1.ChildApplication
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ca); err != nil {
		return fmt.Errorf("childapplication: reading %s: %w", u.GetName(), err)
	}
	o, err := c.observe(ctx, &ca)
	if err != nil {
		return err
	}
	o.deleting = ca.DeletionTimestamp != nil
	if err := c.writeStatus(ctx, dyn, ca.GetName(), statusOf(&ca, o)); err != nil {
		return err
	}
	if ca.DeletionTimestamp != nil {
		if o.workspaceExists && o.orphaned {
			return nil
		}
		return c.releaseFinalizer(ctx, dyn, ca.GetName())
	}
	return c.ensureFinalizer(ctx, dyn, ca.GetName(), o.workspaceExists)
}

func (c *Controller) observe(ctx context.Context, ca *v1alpha1.ChildApplication) (observed, error) {
	var o observed
	if ca.Spec.NetworkRef.Name == "" {
		o.invalid = "spec.network is required"
		return o, nil
	}
	if ca.Spec.App == "" {
		o.invalid = "spec.app is required"
		return o, nil
	}
	if len(ca.Spec.App) > 63 || !dnsLabel.MatchString(ca.Spec.App) {
		o.invalid = fmt.Sprintf("spec.app %q is not a DNS label", ca.Spec.App)
		return o, nil
	}
	placement := ""
	if ca.Spec.Placement != "" {
		if !logicalCluster(ca.Spec.Placement) {
			o.invalid = fmt.Sprintf("spec.placement %q is not a logical cluster path", ca.Spec.Placement)
			return o, nil
		}
		if strings.Contains(ca.Spec.Placement, ":") {
			placement = ca.Spec.Placement
		}
	}

	var appStatus *v1alpha1.NetworkAppStatus
	ndyn, err := c.client(c.networkWorkspace)
	if err != nil {
		return o, err
	}
	nu, err := ndyn.Resource(MarketNetworkGVR).Get(ctx, ca.Spec.NetworkRef.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		o.orphaned = true
		o.orphanReason = "NetworkMissing"
		o.orphanMessage = fmt.Sprintf("the MarketNetwork %s does not exist", ca.Spec.NetworkRef.Name)
	case err != nil:
		return o, err
	default:
		var network v1alpha1.MarketNetwork
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(nu.Object, &network); err != nil {
			return o, fmt.Errorf("childapplication: reading the MarketNetwork %s: %w", ca.Spec.NetworkRef.Name, err)
		}
		listed := false
		for i := range network.Spec.Apps {
			if network.Spec.Apps[i].Name == ca.Spec.App {
				listed = true
				o.stopped = network.Spec.Apps[i].Stopped
				break
			}
		}
		for i := range network.Status.Apps {
			if network.Status.Apps[i].Name == ca.Spec.App {
				appStatus = &network.Status.Apps[i]
				break
			}
		}
		if !listed {
			o.orphaned = true
			o.orphanReason = "AppMissing"
			o.orphanMessage = fmt.Sprintf("the MarketNetwork %s lists no application %s", ca.Spec.NetworkRef.Name, ca.Spec.App)
		}
	}

	workspace := placement
	if appStatus != nil && appStatus.Workspace != "" {
		workspace = appStatus.Workspace
	}
	workspace = cmp.Or(workspace, c.networkWorkspace+":"+ca.Spec.App)
	if !logicalCluster(workspace) {
		o.invalid = fmt.Sprintf("the child workspace %q is not a logical cluster path", workspace)
		return o, nil
	}
	o.workspace = workspace
	parent, name, ok := splitCluster(workspace)
	if !ok {
		o.invalid = fmt.Sprintf("the child workspace %q is not a logical cluster path", workspace)
		return o, nil
	}
	wdyn, err := c.client(parent)
	if err != nil {
		return o, err
	}
	wu, err := wdyn.Resource(WorkspaceGVR).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		list, lerr := wdyn.Resource(WorkspaceGVR).List(ctx, metav1.ListOptions{LabelSelector: appWorkspaceLabel + "=" + ca.Spec.App})
		if lerr != nil && !apierrors.IsNotFound(lerr) && !apierrors.IsForbidden(lerr) {
			return o, lerr
		}
		if lerr == nil {
			for i := range list.Items {
				wu = &list.Items[i]
				o.workspace = parent + ":" + wu.GetName()
				break
			}
		}
	} else if err != nil {
		return o, err
	}
	if wu != nil {
		o.workspaceExists = true
		o.workspaceDeleting = wu.GetDeletionTimestamp() != nil
		phase, _, _ := unstructured.NestedString(wu.Object, "status", "phase")
		o.workspaceReady = phase == "Ready" && !o.workspaceDeleting
	}

	if appStatus != nil && appStatus.Instance != "" {
		o.coreExpected = true
		ref, ready, failed, err := c.observeCore(ctx, appStatus.Instance)
		if err != nil {
			return o, err
		}
		o.coreRef, o.coreReady, o.coreFailed = ref, ready, failed
		o.coreFound = ref != ""
	}

	if o.workspaceExists && o.workspaceReady {
		components, failed, ready, err := c.observeComponents(ctx, o.workspace)
		if err != nil {
			if appStatus == nil {
				return o, err
			}
			components, failed, ready = fallbackComponents(appStatus)
		}
		o.components, o.componentFailed, o.componentsReady = components, failed, ready
	} else if appStatus != nil {
		o.components, o.componentFailed, o.componentsReady = fallbackComponents(appStatus)
	}
	return o, nil
}

func statusOf(ca *v1alpha1.ChildApplication, o observed) v1alpha1.ChildApplicationStatus {
	generation := ca.Generation
	status := v1alpha1.ChildApplicationStatus{ObservedGeneration: generation, Workspace: o.workspace, CoreRef: o.coreRef,
		Components: append([]v1alpha1.ChildApplicationComponent(nil), o.components...)}
	status.Conditions = append([]metav1.Condition(nil), ca.Status.Conditions...)
	sort.Slice(status.Components, func(i, j int) bool {
		if status.Components[i].Kind != status.Components[j].Kind {
			return status.Components[i].Kind < status.Components[j].Kind
		}
		return status.Components[i].Name < status.Components[j].Name
	})
	if len(status.Components) == 0 {
		status.Components = nil
	}

	ready, progressing, failed, orphaned := false, false, false, false
	reason, message := "Reconciling", "the child application is being observed"
	switch {
	case o.deleting && o.orphaned && o.workspaceExists:
		orphaned, reason, message = true, "WorkspaceKept", "the child workspace is kept for its MarketNetwork; remove it there"
	case o.deleting:
		progressing, reason, message = true, "Deleting", "the child workspace belongs to its MarketNetwork and is not deleted"
	case o.invalid != "":
		failed, reason, message = true, "InvalidSpec", o.invalid
	case o.orphaned:
		orphaned, reason, message = true, o.orphanReason, o.orphanMessage
	case o.stopped:
		progressing, reason, message = true, "Stopped", "the MarketNetwork lists this application as stopped"
	case !o.workspaceExists:
		progressing, reason, message = true, "WaitingForWorkspace", "the child workspace does not exist yet"
	case o.workspaceDeleting:
		progressing, reason, message = true, "WorkspaceDeleting", "the child workspace is being deleted"
	case !o.workspaceReady:
		progressing, reason, message = true, "WorkspaceNotReady", "the child workspace is not Ready yet"
	case o.coreFailed:
		failed, reason, message = true, "CoreFailed", "the child core HostProcess failed"
	case o.coreExpected && !o.coreFound:
		progressing, reason, message = true, "WaitingForCore", "the child core HostProcess is not placed yet"
	case o.coreFound && !o.coreReady:
		progressing, reason, message = true, "CoreNotReady", "the child core HostProcess is not Running yet"
	case o.componentFailed:
		failed, reason, message = true, "ComponentFailed", "a child component failed"
	case !o.componentsReady:
		progressing, reason, message = true, "WaitingForComponents", "the child components are not all Ready"
	default:
		ready, reason, message = true, "Ready", "the child workspace, core and components are Ready"
	}

	conds.Set(&status.Conditions, generation, ConditionReady, ready, reason, message)
	conds.Set(&status.Conditions, generation, ConditionProgressing, progressing, reason, message)
	conds.Set(&status.Conditions, generation, ConditionFailed, failed, reason, message)
	conds.Set(&status.Conditions, generation, ConditionOrphaned, orphaned, reason, message)
	sort.Slice(status.Conditions, func(i, j int) bool { return status.Conditions[i].Type < status.Conditions[j].Type })
	return status
}

func (c *Controller) writeStatus(ctx context.Context, dyn dynamic.Interface, name string, status v1alpha1.ChildApplicationStatus) error {
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return fmt.Errorf("childapplication: encoding the status of %s: %w", name, err)
	}
	if object == nil {
		object = map[string]any{}
	}
	ri := dyn.Resource(ChildApplicationGVR)
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		existing, _, _ := unstructured.NestedMap(cur.Object, "status")
		if reflect.DeepEqual(existing, object) {
			return nil
		}
		cur.Object["status"] = object
		if _, err := ri.UpdateStatus(ctx, cur, metav1.UpdateOptions{FieldManager: c.fieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("childapplication: the status of %s stayed in conflict", name)
}

func (c *Controller) ensureFinalizer(ctx context.Context, dyn dynamic.Interface, name string, add bool) error {
	if !add {
		return nil
	}
	ri := dyn.Resource(ChildApplicationGVR)
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if cur.GetDeletionTimestamp() != nil || slices.Contains(cur.GetFinalizers(), Finalizer) {
			return nil
		}
		cur.SetFinalizers(append(cur.GetFinalizers(), Finalizer))
		if _, err := ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: c.fieldManager}); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("childapplication: the finalizer of %s stayed in conflict", name)
}

func (c *Controller) releaseFinalizer(ctx context.Context, dyn dynamic.Interface, name string) error {
	ri := dyn.Resource(ChildApplicationGVR)
	for range 5 {
		cur, err := ri.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !slices.Contains(cur.GetFinalizers(), Finalizer) {
			return nil
		}
		finalizers := make([]string, 0, len(cur.GetFinalizers()))
		for _, f := range cur.GetFinalizers() {
			if f != Finalizer {
				finalizers = append(finalizers, f)
			}
		}
		cur.SetFinalizers(finalizers)
		if _, err := ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: c.fieldManager}); err == nil {
			return nil
		} else if apierrors.IsNotFound(err) {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("childapplication: the finalizer of %s stayed in conflict", name)
}

func logicalCluster(s string) bool {
	if s == "" {
		return false
	}
	for segment := range strings.SplitSeq(s, ":") {
		if len(segment) > 63 || !dnsLabel.MatchString(segment) {
			return false
		}
	}
	return true
}

func splitCluster(s string) (string, string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	parent, name := s[:i], s[i+1:]
	if !logicalCluster(parent) || len(name) > 63 || !dnsLabel.MatchString(name) {
		return "", "", false
	}
	return parent, name, true
}
