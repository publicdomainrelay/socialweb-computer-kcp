package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	ConditionComponentDrift = "ComponentDrift"

	ReasonDriftReverted = "ChildEditReverted"
	ReasonNoDrift       = "NoDrift"
)

type driftState struct {
	children map[string]string

	since time.Time
}

func driftKey(r Root) string { return r.Workspace + "|" + r.Owner() }

func (c *Composer) revertForeignSpec(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, u kube.Doc) error {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return err
	}
	rc := ri.In(docMetaField(u, "namespace"))
	kind, name := docField(u, "kind"), docMetaField(u, "name")
	child := kind + "/" + name
	live, err := kube.Get[kube.Doc](ctx, rc, name)
	if err != nil {
		return fmt.Errorf("compose: reading %s in %s for drift: %w", child, ws, err)
	}
	desired, _ := u["spec"].(map[string]any)
	liveSpec, _ := (*live)["spec"].(map[string]any)
	var drifted []string
	patch := map[string]any{}
	for _, key := range foreignSpecKeys(*live, r.Manager()) {
		want, wanted := desired[key]
		have, present := liveSpec[key]
		switch {
		case !wanted && present:
			patch[key] = nil
		case wanted && !reflect.DeepEqual(normalizeJSON(want), normalizeJSON(have)):
			patch[key] = want
		default:
			continue
		}
		drifted = append(drifted, key)
	}
	if len(drifted) == 0 {
		c.setDrift(r, child, "")
		return nil
	}
	sort.Strings(drifted)
	data, err := json.Marshal(map[string]any{"spec": patch})
	if err != nil {
		return err
	}
	if _, err := kube.Patch[kube.Doc](ctx, rc, name, types.MergePatchType, data, metav1.PatchOptions{FieldManager: r.Manager()}); err != nil {
		return fmt.Errorf("compose: reverting the edited spec of %s in %s: %w", child, ws, err)
	}
	message := fmt.Sprintf("%s: spec.%s set by another manager is reverted to the spec %s %s composes", child,
		strings.Join(drifted, ", spec."), r.Kind, r.Name)
	c.log.Warn("a direct edit of a composed child is reverted", "root", r.Owner(), "child", child, "workspace", ws, "fields", drifted)
	c.Event(ctx, r, ws, docField(u, "apiVersion"), kind, name, "Warning", ReasonChildEditReverted, message)
	c.setDrift(r, child, message)
	return nil
}

func foreignSpecKeys(live kube.Doc, manager string) []string {
	meta, _ := live["metadata"].(map[string]any)
	entries, _ := meta["managedFields"].([]any)
	seen := map[string]bool{}
	var out []string
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry == nil || entry["manager"] == manager || entry["subresource"] == "status" {
			continue
		}
		fields, _ := entry["fieldsV1"].(map[string]any)
		spec, _ := fields["f:spec"].(map[string]any)
		for key := range spec {
			name, ok := strings.CutPrefix(key, "f:")
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func normalizeJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

func (c *Composer) setDrift(r Root, child, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.drift == nil {
		c.drift = map[string]*driftState{}
	}
	key := driftKey(r)
	state := c.drift[key]
	if message == "" {
		if state != nil {
			delete(state.children, child)
			if len(state.children) == 0 {
				delete(c.drift, key)
			}
		}
		return
	}
	if state == nil {
		state = &driftState{children: map[string]string{}, since: time.Now().UTC()}
		c.drift[key] = state
	}
	state.children[child] = message
}

func (c *Composer) driftCondition(r Root, gen int64) map[string]any {
	c.mu.Lock()
	state := c.drift[driftKey(r)]
	var messages []string
	since := time.Now().UTC()
	if state != nil {
		for _, m := range state.children {
			messages = append(messages, m)
		}
		since = state.since
	}
	c.mu.Unlock()
	cond := map[string]any{"type": ConditionComponentDrift, "observedGeneration": gen,
		"lastTransitionTime": since.Format(time.RFC3339)}
	if len(messages) == 0 {
		cond["status"], cond["reason"], cond["message"] = string(metav1.ConditionFalse), ReasonNoDrift, "every composed child matches its desired spec"
		return cond
	}
	sort.Strings(messages)
	cond["status"], cond["reason"], cond["message"] = string(metav1.ConditionTrue), ReasonDriftReverted, strings.Join(messages, "; ")
	return cond
}

func withDriftCondition(m map[string]any, cond map[string]any) {
	list, _ := m["conditions"].([]any)
	out := make([]any, 0, len(list)+1)
	replaced := false
	for _, raw := range list {
		existing, _ := raw.(map[string]any)
		if existing != nil && existing["type"] == ConditionComponentDrift {
			if existing["status"] == cond["status"] {
				cond["lastTransitionTime"] = existing["lastTransitionTime"]
			}
			out = append(out, cond)
			replaced = true
			continue
		}
		out = append(out, raw)
	}
	if !replaced {
		out = append(out, cond)
	}
	m["conditions"] = out
}
