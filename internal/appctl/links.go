package appctl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
)

const ComponentKindComponentLink = "ComponentLink"

var ComponentLinkGVR = exposurev1alpha1.ComponentLinkGVR

type desiredLink struct {
	Name string

	Spec exposurev1alpha1.ComponentLinkSpec
}

type specRef struct {
	Kind string

	Name string
}

func (ctl *Controller) ensureDesiredLinks(ctx context.Context, root *unstructured.Unstructured, desired []desiredComponent) error {
	for _, link := range componentLinks(ctl.c.workspace(), desired) {
		if err := ctl.ensureLink(ctx, root, link); err != nil {
			return fmt.Errorf("ComponentLink %s: %w", link.Name, err)
		}
	}
	return nil
}

func (ctl *Controller) pruneDesiredLinks(ctx context.Context, owner string, desired []desiredComponent) error {
	want := map[string]bool{}
	for _, link := range componentLinks(ctl.c.workspace(), desired) {
		want[link.Name] = true
	}
	ri, err := ctl.client(ctl.c.workspace(), ComponentLinkGVR)
	if err != nil {
		return err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		name := list.Items[i].GetName()
		if want[name] {
			continue
		}
		if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (ctl *Controller) deleteLinks(ctx context.Context, owner string) error {
	ri, err := ctl.client(ctl.c.workspace(), ComponentLinkGVR)
	if err != nil {
		return err
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range list.Items {
		if err := ri.Delete(ctx, list.Items[i].GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (ctl *Controller) ensureLink(ctx context.Context, root *unstructured.Unstructured, link desiredLink) error {
	if root.GetUID() == "" {
		return fmt.Errorf("appctl: the %s %s has no uid; ensure it before its links", root.GetKind(), root.GetName())
	}
	desiredSpec, err := componentSpecMap(link.Spec)
	if err != nil {
		return err
	}
	ri, err := ctl.client(ctl.c.workspace(), ComponentLinkGVR)
	if err != nil {
		return err
	}
	cur, err := ri.Get(ctx, link.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		want := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": exposurev1alpha1.GroupVersion.String(), "kind": ComponentKindComponentLink}}
		want.SetName(link.Name)
		want.SetLabels(componentLabels(root, ComponentKindComponentLink))
		want.SetOwnerReferences([]metav1.OwnerReference{ownerRefOf(root)})
		if desiredSpec != nil {
			want.Object["spec"] = desiredSpec
		}
		_, err = ri.Create(ctx, want, metav1.CreateOptions{FieldManager: FieldManager})
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	changed := false
	labels := cur.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range componentLabels(root, ComponentKindComponentLink) {
		if labels[k] != v {
			labels[k] = v
			changed = true
		}
	}
	cur.SetLabels(labels)
	if refs, refChanged := componentOwnerRefs(cur.GetOwnerReferences(), ownerRefOf(root)); refChanged {
		cur.SetOwnerReferences(refs)
		changed = true
	}
	if desiredSpec != nil && !reflect.DeepEqual(cur.Object["spec"], desiredSpec) {
		cur.Object["spec"] = desiredSpec
		changed = true
	}
	if !changed {
		return nil
	}
	_, err = ri.Update(ctx, cur, metav1.UpdateOptions{FieldManager: FieldManager})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("appctl: the ComponentLink %s changed under its update; the next pass retries", link.Name)
	}
	return err
}

func componentLinks(workspace string, desired []desiredComponent) []desiredLink {
	out := map[string]desiredLink{}
	for _, d := range desired {
		if d.Kind == ComponentKindService || d.Kind == ComponentKindExposure {
			continue
		}
		for _, ref := range specRefs(d.Spec) {
			if !isLinkTargetKind(ref.Kind) || ref.Name == "*" {
				continue
			}
			if strings.EqualFold(d.Kind, ref.Kind) && strings.EqualFold(d.Name, ref.Name) {
				continue
			}
			spec := exposurev1alpha1.ComponentLinkSpec{
				From:         exposurev1alpha1.ComponentLinkFrom{Ref: &commonv1.ObjectRef{Workspace: workspace, Kind: d.Kind, Name: d.Name}},
				To:           commonv1.ObjectRef{Workspace: workspace, Kind: ref.Kind, Name: ref.Name},
				Capabilities: []exposurev1alpha1.LinkCapability{{Name: bus.CapabilityXRPC}, {Name: bus.CapabilityStream}},
			}
			name := componentLinkName(d.Kind, d.Name, ref.Kind, ref.Name)
			out[name] = desiredLink{Name: name, Spec: spec}
		}
	}
	names := make([]string, 0, len(out))
	for name := range out {
		names = append(names, name)
	}
	sort.Strings(names)
	links := make([]desiredLink, 0, len(names))
	for _, name := range names {
		links = append(links, out[name])
	}
	return links
}

func componentLinkName(fromKind, fromName, toKind, toName string) string {
	raw := strings.ToLower("link-" + fromKind + "-" + fromName + "-" + toKind + "-" + toName)
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			return r
		default:
			return '-'
		}
	}, raw)
	safe = strings.Trim(safe, "-.")
	if safe == "" {
		return "link"
	}
	if len(safe) <= 63 {
		return safe
	}
	sum := sha256.Sum256([]byte(raw))
	return strings.TrimRight(safe[:54], "-.") + "-" + hex.EncodeToString(sum[:4])
}

func isLinkTargetKind(kind string) bool {
	if strings.EqualFold(kind, ComponentKindPolicyEngine) {
		return false
	}
	for candidate := range ComponentGVRs {
		if strings.EqualFold(candidate, kind) {
			return true
		}
	}
	return false
}

func specRefs(spec any) []specRef {
	value, err := componentSpecMap(spec)
	if err != nil || value == nil {
		return nil
	}
	var out []specRef
	walkSpecRefs(value, &out)
	return out
}

func walkSpecRefs(value any, out *[]specRef) {
	switch typed := value.(type) {
	case map[string]any:
		for key, entry := range typed {
			if isSpecRefKey(key) {
				appendSpecRefs(entry, out)
			}
			walkSpecRefs(entry, out)
		}
	case []any:
		for _, entry := range typed {
			walkSpecRefs(entry, out)
		}
	}
}

func isSpecRefKey(key string) bool {
	return strings.EqualFold(key, "ref") || strings.HasSuffix(key, "Ref") || strings.HasSuffix(key, "Refs")
}

func appendSpecRefs(value any, out *[]specRef) {
	switch typed := value.(type) {
	case map[string]any:
		kind, _ := typed["kind"].(string)
		name, _ := typed["name"].(string)
		kind, name = strings.TrimSpace(kind), strings.TrimSpace(name)
		if kind == "" || name == "" {
			return
		}
		*out = append(*out, specRef{Kind: kind, Name: name})
	case []any:
		for _, entry := range typed {
			appendSpecRefs(entry, out)
		}
	}
}
