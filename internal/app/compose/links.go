package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"
)

type Link struct {
	Name string

	Spec exposurev1alpha1.ComponentLinkSpec
}

func (l Link) Workspace() string { return strings.TrimSpace(l.Spec.To.Workspace) }

func LinkName(parts ...string) string {
	raw := strings.ToLower("link-" + strings.Join(parts, "-"))
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

func Cap(name string, scope ...string) exposurev1alpha1.LinkCapability {
	return exposurev1alpha1.LinkCapability{Name: name, Scope: append([]string(nil), scope...)}
}

func RefLink(from, to commonv1.ObjectRef, caps ...exposurev1alpha1.LinkCapability) Link {
	f := from
	return Link{Name: LinkName(from.Kind, from.Name, to.Kind, to.Name), Spec: exposurev1alpha1.ComponentLinkSpec{
		From: exposurev1alpha1.ComponentLinkFrom{Ref: &f}, To: to, Capabilities: caps}}
}

func SelectorLink(name string, from exposurev1alpha1.ComponentLinkSelector, to commonv1.ObjectRef, caps ...exposurev1alpha1.LinkCapability) Link {
	f := from
	return Link{Name: name, Spec: exposurev1alpha1.ComponentLinkSpec{
		From: exposurev1alpha1.ComponentLinkFrom{Selector: &f}, To: to, Capabilities: caps}}
}

func Ref(group, kind, name, ws string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: group, Kind: kind, Name: name, Workspace: ws}
}

func SpecRefLinks(ws string, desired []Desired) []Link {
	out := map[string]Link{}
	for _, d := range desired {
		if IsExposureKind(d.Kind) {
			continue
		}
		for _, ref := range specRefs(d.Spec) {
			if !isLinkTargetKind(ref.Kind) || ref.Name == "*" {
				continue
			}
			if strings.EqualFold(d.Kind, ref.Kind) && strings.EqualFold(d.Name, ref.Name) {
				continue
			}
			link := RefLink(commonv1.ObjectRef{Workspace: ws, Kind: d.Kind, Name: d.Name}, commonv1.ObjectRef{Workspace: ws, Kind: ref.Kind, Name: ref.Name},
				Cap(bus.CapabilityXRPC), Cap(bus.CapabilityStream))
			out[link.Name] = link
		}
	}
	return sortedLinks(out)
}

func sortedLinks(in map[string]Link) []Link {
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	links := make([]Link, 0, len(names))
	for _, name := range names {
		links = append(links, in[name])
	}
	return links
}

func Dedupe(links []Link) []Link {
	out := map[string]Link{}
	for _, l := range links {
		if prev, ok := out[l.Name]; ok {
			prev.Spec.Capabilities = append(prev.Spec.Capabilities, l.Spec.Capabilities...)
			out[l.Name] = prev
			continue
		}
		out[l.Name] = l
	}
	return sortedLinks(out)
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

type specRef struct {
	Kind string

	Name string
}

func specRefs(spec any) []specRef {
	value, err := SpecMap(spec)
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

func (c *Composer) ApplyLinks(ctx context.Context, r Root, set string, links []Link, prune ...string) error {
	keep := map[string]map[string]bool{}
	workspaces := map[string]bool{r.Workspace: true}
	for _, ws := range prune {
		if ws = strings.TrimSpace(ws); ws != "" {
			workspaces[ws] = true
		}
	}
	for _, l := range Dedupe(links) {
		ws := workspaceOr(l.Workspace(), r.Workspace)
		l.Spec.To.Workspace = ws
		workspaces[ws] = true
		if keep[ws] == nil {
			keep[ws] = map[string]bool{}
		}
		keep[ws][l.Name] = true
		obj := &exposurev1alpha1.ComponentLink{ObjectMeta: metav1.ObjectMeta{Name: l.Name, Labels: map[string]string{LinkSetLabel: set}}, Spec: l.Spec}
		if err := c.ApplyTyped(ctx, r, ws, ComponentLinkGVR, exposurev1alpha1.GroupVersion.String(), ComponentKindComponentLink, obj); err != nil {
			return fmt.Errorf("ComponentLink %s: %w", l.Name, err)
		}
	}
	selector := OwnerLabel + "=" + r.Owner() + "," + LinkSetLabel + "=" + set
	for ws := range workspaces {
		if err := c.Prune(ctx, ws, ComponentLinkGVR, selector, keep[ws]); err != nil {
			return err
		}
	}
	return nil
}

func (c *Composer) DeleteLinks(ctx context.Context, r Root, workspaces ...string) error {
	seen := map[string]bool{}
	for _, ws := range append([]string{r.Workspace}, workspaces...) {
		if ws == "" || seen[ws] {
			continue
		}
		seen[ws] = true
		if err := c.Prune(ctx, ws, ComponentLinkGVR, OwnerLabel+"="+r.Owner(), nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Composer) LinksAccepted(ctx context.Context, r Root, links []Link) (bool, string) {
	for _, l := range Dedupe(links) {
		ws := workspaceOr(l.Workspace(), r.Workspace)
		u, err := c.Get(ctx, ws, ComponentLinkGVR, l.Name)
		if err != nil {
			return false, "the ComponentLink " + l.Name + " is not readable yet"
		}
		if !linkAccepted(u) {
			return false, "waiting for the ComponentLink " + l.Name + " in " + ws + " to be Accepted"
		}
	}
	return true, ""
}

func linkAccepted(u *unstructured.Unstructured) bool {
	var link exposurev1alpha1.ComponentLink
	if err := unstruct.Into(u, &link); err != nil {
		return false
	}
	if link.Status.ObservedGeneration < link.Generation {
		return false
	}
	cond := meta.FindStatusCondition(link.Status.Conditions, exposurev1alpha1.ComponentLinkConditionAccepted)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func ExposureLinks(ws string, desired []Desired) []Link {
	targets := map[string]exposurev1alpha1.ServiceSpec{}
	for _, d := range desired {
		if spec, ok := d.Spec.(exposurev1alpha1.ServiceSpec); ok {
			targets[d.Name] = spec
		}
	}
	var out []Link
	for _, d := range desired {
		var from commonv1.ObjectRef
		var svc exposurev1alpha1.ServiceSpec
		switch spec := d.Spec.(type) {
		case exposurev1alpha1.ServiceSpec:
			from, svc = Ref(exposurev1alpha1.GroupName, ComponentKindService, d.Name, ws), spec
		case exposurev1alpha1.ExposureSpec:
			target, ok := targets[spec.ServiceRef]
			if !ok {
				continue
			}
			from, svc = Ref(exposurev1alpha1.GroupName, ComponentKindExposure, d.Name, ws), target
		default:
			continue
		}
		to := svc.TargetRef
		to.Workspace = workspaceOr(to.Workspace, ws)
		handler := strings.TrimSpace(svc.Handler)
		if handler == "" {
			handler = bus.DefaultHandlerName
		}
		out = append(out, RefLink(from, to, Cap(bus.CapabilityStream, handler), Cap(bus.CapabilityXRPC)))
	}
	return out
}

func ComponentLinks(ws string, desired []Desired) []Link {
	return append(SpecRefLinks(ws, desired), ExposureLinks(ws, desired)...)
}

func Existing(snap Snapshot, desired []Desired) []Desired {
	var out []Desired
	for _, d := range desired {
		if _, ok := snap.View(d.Kind, d.Name); ok {
			out = append(out, d)
		}
	}
	return out
}
