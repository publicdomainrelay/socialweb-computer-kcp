package eventstream

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsapi "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const AtprotoRepoKind = "AtprotoRepo"

type SourcePlan struct {
	Name string

	Key string

	SourceRef commonv1.ObjectRef

	RepoRef *commonv1.ObjectRef

	Filter eventsapi.EventStreamFilter
}

type WatchRepo struct {
	DID string

	Ref *commonv1.ObjectRef
}

func EventStreamNameForWatchSource(watchName, sourceKey string) string {
	if strings.HasPrefix(sourceKey, "pds/") {
		return EventStreamNameForWatch(watchName)
	}
	sum := sha256.Sum256([]byte(sourceKey))
	suffix := hex.EncodeToString(sum[:6])
	candidate := EventStreamNamePrefix + watchName + "-" + suffix
	if len(candidate) <= maxEventStreamName {
		return candidate
	}
	keep := maxEventStreamName - len(suffix) - 1
	return candidate[:keep] + "-" + suffix
}

func SpecRepo(spec v1alpha1.AtprotoRecordWatchSpec) (string, error) {
	repo := strings.TrimSpace(spec.Repo)
	switch {
	case strings.HasPrefix(repo, "did:"):
		return repo, nil
	case repo == "" && len(spec.Source.EventSourceRefs) == 0:
		return "", errors.New("eventstream: spec.repo or spec.repoRef is required")
	}
	return "", nil
}

func PlanSources(workspace, watchName string, spec v1alpha1.AtprotoRecordWatchSpec, repo WatchRepo) ([]SourcePlan, error) {
	filter := eventsapi.EventStreamFilter{}
	if collection := strings.TrimSpace(spec.Collection); collection != "" {
		filter.Collections = []string{collection}
	}
	if repo.DID != "" {
		filter.DIDs = []string{repo.DID}
	}
	var plans []SourcePlan
	seen := map[string]bool{}
	for _, ref := range spec.Source.EventSourceRefs {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			continue
		}
		if kind := strings.TrimSpace(ref.Kind); kind != "" && !strings.EqualFold(kind, eventsapi.EventSourceKind) {
			return nil, fmt.Errorf("eventstream: source.eventSourceRefs must name EventSources, not %s %s", kind, name)
		}
		source := commonv1.ObjectRef{Group: eventsapi.GroupName, Kind: eventsapi.EventSourceKind, Name: name, Workspace: ref.InWorkspace(workspace)}
		key := "source/" + source.Workspace + "/" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		plans = append(plans, SourcePlan{Name: EventStreamNameForWatchSource(watchName, key), Key: key, SourceRef: source, Filter: filter})
	}
	if len(plans) > 0 {
		return plans, nil
	}
	if repo.Ref == nil {
		return nil, errors.New("eventstream: the record watch names no source.eventSourceRefs and no AtprotoRepo repoRef")
	}
	key := "pds/" + repo.Ref.Name
	name := EventStreamNameForWatchSource(watchName, key)
	plans = append(plans, SourcePlan{Name: name, Key: key, RepoRef: repo.Ref, Filter: filter,
		SourceRef: commonv1.ObjectRef{Group: eventsapi.GroupName, Kind: eventsapi.EventSourceKind, Name: name, Workspace: workspace}})
	return plans, nil
}

func EventStreamFor(owner metav1.OwnerReference, workspace string, plan SourcePlan) *eventsapi.EventStream {
	source := plan.SourceRef
	if source.Workspace == workspace {
		source.Workspace = ""
	}
	return &eventsapi.EventStream{
		TypeMeta: metav1.TypeMeta{APIVersion: eventsapi.GroupVersion.String(), Kind: "EventStream"},
		ObjectMeta: metav1.ObjectMeta{Name: plan.Name, Labels: map[string]string{RecordWatchLabel: owner.Name},
			OwnerReferences: []metav1.OwnerReference{owner}},
		Spec: eventsapi.EventStreamSpec{
			SourceRef: source,
			Filter:    plan.Filter,
			Sink:      eventsapi.EventStreamSink{Kind: eventsapi.EventStreamSinkRecordWatch, Name: owner.Name, Namespace: workspace},
		},
	}
}

func PdsSourcesFor(owner metav1.OwnerReference, workspace string, plan SourcePlan) (*eventsapi.PdsEventSource, *eventsapi.EventSource, bool) {
	if plan.RepoRef == nil {
		return nil, nil, false
	}
	repo := *plan.RepoRef
	if repo.Workspace == workspace {
		repo.Workspace = ""
	}
	name := plan.SourceRef.Name
	labels := map[string]string{RecordWatchLabel: owner.Name}
	owners := []metav1.OwnerReference{owner}
	backend := &eventsapi.PdsEventSource{TypeMeta: metav1.TypeMeta{APIVersion: eventsapi.GroupVersion.String(), Kind: eventsapi.PdsEventSourceKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels, OwnerReferences: owners}, Spec: eventsapi.PdsEventSourceSpec{RepoRef: repo}}
	abstract := &eventsapi.EventSource{TypeMeta: metav1.TypeMeta{APIVersion: eventsapi.GroupVersion.String(), Kind: eventsapi.EventSourceKind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: maps.Clone(labels), OwnerReferences: owners},
		Spec:       eventsapi.EventSourceSpec{BackendRef: commonv1.ObjectRef{Group: eventsapi.GroupName, Kind: eventsapi.PdsEventSourceKind, Name: name}}}
	return backend, abstract, true
}
