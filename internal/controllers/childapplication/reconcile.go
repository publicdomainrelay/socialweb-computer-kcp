package childapplication

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type observed struct {
	workspace string

	watched []string

	instance string

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

func (c *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, ca *v1alpha1.ChildApplication) (ctlruntime.Result, error) {
	o, err := c.observe(ctx, req, ca)
	c.index(req, refs{network: networkKey{cluster: req.Cluster, name: ca.Spec.NetworkRef.Name}, instance: o.instance, workspaces: o.watched})
	if err != nil {
		return ctlruntime.Result{}, err
	}
	o.deleting = !ca.DeletionTimestamp.IsZero()
	next := ca.DeepCopy()
	next.Status = statusOf(ca, o)
	if err := req.ApplyStatus(ctx, next); err != nil {
		return ctlruntime.Result{}, err
	}
	return ctlruntime.Result{Waiting: o.deleting && o.workspaceExists && o.orphaned}, nil
}

func (c *Controller) observe(ctx context.Context, req ctlruntime.Request, ca *v1alpha1.ChildApplication) (observed, error) {
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
	var network v1alpha1.MarketNetwork
	err := req.Get(ctx, "", ca.Spec.NetworkRef.Name, &network)
	switch {
	case apierrors.IsNotFound(err):
		o.orphaned = true
		o.orphanReason = "NetworkMissing"
		o.orphanMessage = fmt.Sprintf("the MarketNetwork %s does not exist", ca.Spec.NetworkRef.Name)
	case err != nil:
		return o, err
	default:
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
	if appStatus != nil {
		o.instance = appStatus.Instance
	}
	workspace = cmp.Or(workspace, req.Workspace+":"+ca.Spec.App)
	if !logicalCluster(workspace) {
		o.invalid = fmt.Sprintf("the child workspace %q is not a logical cluster path", workspace)
		return o, nil
	}
	o.workspace = workspace
	o.watched = []string{workspace}
	parent, name, ok := splitCluster(workspace)
	if !ok {
		o.invalid = fmt.Sprintf("the child workspace %q is not a logical cluster path", workspace)
		return o, nil
	}
	ws, found, err := c.workspaces.Get(ctx, parent, name)
	if err != nil {
		return o, err
	}
	if !found {
		ws, found, err = c.workspaces.ByLabel(ctx, parent, AppWorkspaceLabel, ca.Spec.App)
		if err != nil && !absent(err) {
			return o, err
		}
		if found {
			o.workspace = parent + ":" + ws.Name
			o.watched = append(o.watched, o.workspace)
		}
	}
	if found {
		o.workspaceExists = true
		o.workspaceDeleting = ws.Deleting
		o.workspaceReady = ws.Phase == "Ready" && !ws.Deleting
	}

	if o.instance != "" {
		o.coreExpected = true
		ref, ready, failed, err := c.observeCore(ctx, o.instance)
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

func absent(err error) bool {
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || meta.IsNoMatchError(err)
}

func statusOf(ca *v1alpha1.ChildApplication, o observed) v1alpha1.ChildApplicationStatus {
	generation := ca.Generation
	status := v1alpha1.ChildApplicationStatus{ObservedGeneration: generation, Workspace: o.workspace, CoreRef: o.coreRef,
		Components: append([]v1alpha1.ChildApplicationComponent(nil), o.components...)}
	status.Conditions = append([]metav1.Condition(nil), ca.Status.Conditions...)
	sortComponents(status.Components)
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
