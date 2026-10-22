package guestidentity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const FieldManagerGuestIdentityGrant = "guest-identity-grants-kcp"

type KCPGrants struct {
	Res dynamic.ResourceInterface

	FieldManager string

	Log *slog.Logger
}

func NewKCPGrants(res dynamic.ResourceInterface) *KCPGrants {
	return &KCPGrants{Res: res, FieldManager: FieldManagerGuestIdentityGrant}
}

func (k *KCPGrants) List(ctx context.Context) ([]v1alpha1.GuestIdentityGrant, error) {
	if k == nil || k.Res == nil {
		return nil, errors.New("guestidentity: the grant store has no GuestIdentityGrant resource")
	}
	list, err := k.Res.List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.GuestIdentityGrant, 0, len(list.Items))
	for i := range list.Items {
		var g v1alpha1.GuestIdentityGrant
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &g); err != nil {
			return nil, fmt.Errorf("guestidentity: decoding GuestIdentityGrant %s: %w", list.Items[i].GetName(), err)
		}
		out = append(out, g)
	}
	return out, nil
}

func (k *KCPGrants) Apply(ctx context.Context, g v1alpha1.GuestIdentityGrant) error {
	if k == nil || k.Res == nil {
		return errors.New("guestidentity: the grant store has no GuestIdentityGrant resource")
	}
	if g.Name == "" {
		return errors.New("guestidentity: applying a GuestIdentityGrant with no name")
	}
	g.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "GuestIdentityGrant"}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&g)
	if err != nil {
		return err
	}
	delete(m, "status")
	u := &unstructured.Unstructured{Object: m}
	fieldManager := k.FieldManager
	if fieldManager == "" {
		fieldManager = FieldManagerGuestIdentityGrant
	}
	if _, err := k.Res.Apply(ctx, g.Name, u, metav1.ApplyOptions{FieldManager: fieldManager, Force: true}); err != nil {
		return err
	}
	return nil
}

func (k *KCPGrants) Delete(ctx context.Context, name string) error {
	if k == nil || k.Res == nil {
		return errors.New("guestidentity: the grant store has no GuestIdentityGrant resource")
	}
	err := k.Res.Delete(ctx, name, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
