package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

type appliedChild struct {
	digest string

	uid types.UID

	live string
}

type applyCache struct {
	mu sync.Mutex

	applied map[string]appliedChild

	live map[types.UID]string

	keys map[types.UID]map[string]bool

	kinds map[string]bool
}

func applyKey(ws string, gvr schema.GroupVersionResource, namespace, name, manager string) string {
	return ws + "|" + gvr.Group + "/" + gvr.Resource + "|" + namespace + "/" + name + "|" + manager
}

func applyDigest(u kube.Doc) string {
	raw, err := json.Marshal(u)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func liveFingerprint(o metav1.Object) string {
	if o == nil || o.GetUID() == "" || o.GetDeletionTimestamp() != nil {
		return ""
	}
	type field struct {
		Manager   string `json:"m"`
		Operation string `json:"o"`
		Fields    any    `json:"f,omitempty"`
	}
	var fields []field
	for _, e := range o.GetManagedFields() {
		if e.Subresource != "" {
			continue
		}
		f := field{Manager: e.Manager, Operation: string(e.Operation)}
		if e.FieldsV1 != nil && len(e.FieldsV1.Raw) > 0 {
			var v any
			if err := json.Unmarshal(e.FieldsV1.Raw, &v); err == nil {
				f.Fields = v
			}
		}
		fields = append(fields, f)
	}
	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].Manager != fields[j].Manager {
			return fields[i].Manager < fields[j].Manager
		}
		return fields[i].Operation < fields[j].Operation
	})
	raw, err := json.Marshal(struct {
		Generation int64                   `json:"g"`
		Labels     map[string]string       `json:"l,omitempty"`
		Owners     []metav1.OwnerReference `json:"o,omitempty"`
		Finalizers []string                `json:"x,omitempty"`
		Fields     []field                 `json:"f,omitempty"`
	}{o.GetGeneration(), o.GetLabels(), o.GetOwnerReferences(), o.GetFinalizers(), fields})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func docObject(d kube.Doc) metav1.Object {
	o, err := kube.Decode[metav1.PartialObjectMetadata](d)
	if err != nil {
		return nil
	}
	return o
}

func (a *applyCache) current(key, digest string) bool {
	if digest == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	got, ok := a.applied[key]
	if !ok || got.digest != digest || got.live == "" {
		return false
	}
	live, seen := a.live[got.uid]
	return seen && live == got.live
}

func (a *applyCache) watch(kind string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kinds == nil {
		a.kinds = map[string]bool{}
	}
	a.kinds[kind] = true
}

func (a *applyCache) record(key, kind, digest string, obj metav1.Object) {
	fp := liveFingerprint(obj)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.applied == nil {
		a.applied = map[string]appliedChild{}
		a.keys = map[types.UID]map[string]bool{}
	}
	if digest == "" || fp == "" || !a.kinds[kind] {
		delete(a.applied, key)
		return
	}
	uid := obj.GetUID()
	a.applied[key] = appliedChild{digest: digest, uid: uid, live: fp}
	if a.keys[uid] == nil {
		a.keys[uid] = map[string]bool{}
	}
	a.keys[uid][key] = true
}

func (a *applyCache) drop(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.applied, key)
}

func (a *applyCache) observe(obj metav1.Object, gone bool) {
	uid := obj.GetUID()
	if uid == "" {
		return
	}
	var fp string
	if !gone {
		fp = liveFingerprint(obj)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.live == nil {
		a.live = map[types.UID]string{}
	}
	if fp != "" {
		a.live[uid] = fp
		return
	}
	delete(a.live, uid)
	for key := range a.keys[uid] {
		if a.applied[key].uid == uid {
			delete(a.applied, key)
		}
	}
	delete(a.keys, uid)
}
