package statesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (c *Controller) acknowledge(ctx context.Context, dyn dynamic.Interface, obj *v1alpha1.StateSync, observation Observation) error {
	ack := obj.Spec.Ack
	if ack == nil || len(ack.Paths) == 0 {
		return nil
	}
	source := obj.Spec.Source
	if source == nil {
		return errors.New("statesync: ack.paths need spec.source")
	}
	workspace := strings.TrimSpace(source.Workspace)
	if workspace != "" && workspace != c.workspace {
		return fmt.Errorf("statesync: the ack source workspace %s is not the controller workspace %s", workspace, c.workspace)
	}
	name := strings.TrimSpace(source.Name)
	if name == "" {
		return errors.New("statesync: the ack source has no name")
	}
	groupVersion, err := schema.ParseGroupVersion(strings.TrimSpace(source.ApiVersion))
	if err != nil {
		return fmt.Errorf("statesync: source.apiVersion %q is invalid: %w", source.ApiVersion, err)
	}
	resource := pluralResource(source.Kind)
	if resource == "" {
		return errors.New("statesync: the ack source has no kind")
	}
	ri := dyn.Resource(schema.GroupVersionResource{Group: groupVersion.Group, Version: groupVersion.Version, Resource: resource})
	current, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if source.UID != "" && string(current.GetUID()) != source.UID {
		return fmt.Errorf("statesync: the ack source %s has UID %s, not %s", name, current.GetUID(), source.UID)
	}
	patch, err := ackMergePatch(current.Object, ack.Paths, observation.Meta)
	if err != nil {
		return err
	}
	if len(patch) == 0 {
		return nil
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	fieldManager := strings.TrimSpace(ack.FieldManager)
	if fieldManager == "" {
		fieldManager = FieldManager
	}
	if _, err := ri.Patch(ctx, name, types.MergePatchType, body, metav1.PatchOptions{FieldManager: fieldManager}, "status"); err != nil {
		return fmt.Errorf("statesync: patching the status of %s: %w", name, err)
	}
	return nil
}

func ackMergePatch(object map[string]any, paths []string, meta map[string]string) (map[string]any, error) {
	tree := map[string]any{}
	for _, path := range paths {
		segments, err := splitAckPath(path)
		if err != nil {
			return nil, err
		}
		value, ok := meta[segments[len(segments)-1]]
		if !ok {
			continue
		}
		insertPath(tree, segments, value)
	}
	if len(tree) == 0 {
		return nil, nil
	}
	statusObject, found, err := unstructured.NestedMap(object, "status")
	if err != nil {
		return nil, err
	}
	if found && statusObject != nil {
		if err := checkMergeParents(tree, statusObject); err != nil {
			return nil, err
		}
	}
	return map[string]any{"status": tree}, nil
}

func checkMergeParents(tree map[string]any, current map[string]any) error {
	for key, value := range tree {
		subtree, nested := value.(map[string]any)
		if !nested {
			continue
		}
		child, ok := current[key]
		if !ok || child == nil {
			continue
		}
		childMap, ok := child.(map[string]any)
		if !ok {
			return fmt.Errorf("statesync: the ack path parent %s is not an object", key)
		}
		if err := checkMergeParents(subtree, childMap); err != nil {
			return err
		}
	}
	return nil
}

func insertPath(tree map[string]any, segments []string, value string) {
	current := tree
	for i, segment := range segments {
		if i == len(segments)-1 {
			current[segment] = value
			return
		}
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = map[string]any{}
			current[segment] = next
		}
		current = next
	}
}

func splitAckPath(path string) ([]string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("statesync: an ack path is empty")
	}
	segments := strings.Split(trimmed, ".")
	if slices.Contains(segments, "") {
		return nil, fmt.Errorf("statesync: ack path %q has an empty segment", path)
	}
	return segments, nil
}

func pluralResource(kind string) string {
	name := strings.ToLower(strings.TrimSpace(kind))
	switch name {
	case "":
		return ""
	case "pds":
		return "pds"
	}
	if strings.HasSuffix(name, "s") || strings.HasSuffix(name, "x") || strings.HasSuffix(name, "z") ||
		strings.HasSuffix(name, "ch") || strings.HasSuffix(name, "sh") {
		return name + "es"
	}
	if strings.HasSuffix(name, "y") && len(name) > 1 {
		previous := name[len(name)-2]
		if !strings.ContainsRune("aeiou", rune(previous)) {
			return name[:len(name)-1] + "ies"
		}
	}
	return name + "s"
}
