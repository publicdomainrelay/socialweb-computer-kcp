package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clicfg"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/stack/componentcore"
)

type linkRow struct {
	Workspace    string                            `json:"workspace"`
	Name         string                            `json:"name"`
	From         string                            `json:"from"`
	To           string                            `json:"to"`
	Capabilities []exposurev1alpha1.LinkCapability `json:"capabilities"`
	Suspend      bool                              `json:"suspend,omitempty"`
	Accepted     metav1.ConditionStatus            `json:"accepted,omitempty"`
	ResolvedRefs metav1.ConditionStatus            `json:"resolvedRefs,omitempty"`
	Message      string                            `json:"message,omitempty"`
}

func linkFrom(f exposurev1alpha1.ComponentLinkFrom) string {
	if f.Ref != nil {
		return f.Ref.String()
	}
	s := f.Selector
	if s == nil {
		return "-"
	}
	out := s.Kind
	if s.Group != "" {
		out += "." + s.Group
	}
	if s.Workspace != "" {
		out = s.Workspace + ":" + out
	}
	var terms []string
	for _, k := range slices.Sorted(maps.Keys(s.MatchLabels)) {
		terms = append(terms, k+"="+s.MatchLabels[k])
	}
	for _, e := range s.MatchExpressions {
		terms = append(terms, e.Key+":"+string(e.Operator)+"("+strings.Join(e.Values, "|")+")")
	}
	return out + "/*[" + strings.Join(terms, ",") + "]"
}

func linkCapabilities(caps []exposurev1alpha1.LinkCapability) string {
	var out []string
	for _, c := range caps {
		if len(c.Scope) == 0 {
			out = append(out, c.Name)
			continue
		}
		out = append(out, c.Name+"("+strings.Join(c.Scope, "|")+")")
	}
	return cmp.Or(strings.Join(out, ","), "-")
}

func newLinkRow(ws string, l exposurev1alpha1.ComponentLink) linkRow {
	r := linkRow{Workspace: ws, Name: l.Name, From: linkFrom(l.Spec.From), To: l.Spec.To.String(),
		Capabilities: l.Spec.Capabilities, Suspend: l.Spec.Suspend}
	var msgs []string
	for _, t := range []string{exposurev1alpha1.ComponentLinkConditionAccepted, exposurev1alpha1.ComponentLinkConditionResolvedRefs} {
		c := meta.FindStatusCondition(l.Status.Conditions, t)
		if c == nil {
			msgs = append(msgs, t+": not reported")
			continue
		}
		if t == exposurev1alpha1.ComponentLinkConditionAccepted {
			r.Accepted = c.Status
		} else {
			r.ResolvedRefs = c.Status
		}
		if c.Status != metav1.ConditionTrue {
			msgs = append(msgs, t+": "+cmp.Or(c.Message, c.Reason))
		}
	}
	if l.Spec.Suspend {
		msgs = append([]string{"suspended"}, msgs...)
	}
	r.Message = strings.Join(msgs, "; ")
	return r
}

func runDevLinks(ctx context.Context, a *app, vals clicfg.Values, rest []string) int {
	if len(rest) == 2 {
		vals = vals.With("instance", rest[0])
		rest = rest[1:]
	}
	if len(rest) > 1 {
		return a.fail(exitRefused, "dev links: dev links [<instance>] [<workspace>] [--json]")
	}
	l, rec, code := a.record("dev links", vals)
	if code != exitOK {
		return code
	}
	if rec.AdminKubeconfig == "" {
		return a.fail(exitFailed, "dev links: instance %s records no admin kubeconfig; dev up first", l.ID)
	}
	base := baseWorkspace(rec)
	workspaces := networkWorkspaces(ctx, rec)
	if len(rest) == 1 {
		workspaces = []string{workspaceName(base, rest[0])}
	}
	if len(workspaces) == 0 {
		workspaces = componentcore.AppWorkspaces(base)
	}
	rows := []linkRow{}
	for _, ws := range workspaces {
		c, err := workspaceClient(rec.AdminKubeconfig, ws)
		if err != nil {
			return a.fail(exitFailed, "dev links: %v", err)
		}
		links, err := kube.List[exposurev1alpha1.ComponentLink](ctx, c.Resource(exposurev1alpha1.ComponentLinkGVR), metav1.ListOptions{})
		if absentResource(err) {
			continue
		}
		if err != nil {
			return a.fail(exitFailed, "dev links: listing componentlinks in %s: %v", ws, err)
		}
		for _, link := range links {
			rows = append(rows, newLinkRow(ws, link))
		}
	}
	if vals.Bool("json") {
		a.printJSON(rows)
		return exitOK
	}
	for _, r := range rows {
		fmt.Fprintf(a.stdout, "%-40s %-48s -> %-48s accepted=%-7s resolvedRefs=%-7s %s %s\n", r.Name, r.From, r.To,
			cmp.Or(string(r.Accepted), "-"), cmp.Or(string(r.ResolvedRefs), "-"), linkCapabilities(r.Capabilities), r.Message)
	}
	if len(rows) == 0 {
		fmt.Fprintf(a.stdout, "no ComponentLinks in %s of instance %s\n", strings.Join(workspaces, ", "), l.ID)
	}
	return exitOK
}
