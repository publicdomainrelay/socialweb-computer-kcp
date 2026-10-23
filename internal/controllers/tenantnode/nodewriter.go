package tenantnode

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type ClientNodeWriter struct {
	client kubernetes.Interface
}

func NewClientNodeWriter(client kubernetes.Interface) *ClientNodeWriter {
	return &ClientNodeWriter{client: client}
}

func (w *ClientNodeWriter) PatchProviderID(ctx context.Context, nodeName, providerID string) error {
	body, err := json.Marshal(map[string]any{
		"spec": map[string]any{"providerID": providerID},
	})
	if err != nil {
		return fmt.Errorf("tenantnode: encoding the provider ID %s for the Node %s: %w",
			providerID, nodeName, err)
	}
	if _, err := w.client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("tenantnode: patching the provider ID of the Node %s: %w", nodeName, err)
	}
	return nil
}

func (w *ClientNodeWriter) SetLabels(ctx context.Context, nodeName string, set map[string]string, remove []string) error {
	labels := map[string]any{}
	for k, v := range set {
		labels[k] = v
	}
	for _, k := range remove {
		labels[k] = nil
	}
	if len(labels) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"labels": labels},
	})
	if err != nil {
		return fmt.Errorf("tenantnode: encoding the labels of the Node %s: %w", nodeName, err)
	}
	if _, err := w.client.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, body, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("tenantnode: patching the labels of the Node %s: %w", nodeName, err)
	}
	return nil
}

func (w *ClientNodeWriter) DeleteNode(ctx context.Context, nodeName string) error {
	err := w.client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("tenantnode: deleting the Node %s: %w", nodeName, err)
	}
	return nil
}
