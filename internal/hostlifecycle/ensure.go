package hostlifecycle

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	KcpInstallationName = "host-kcp"
	HostInstallName     = "host-install"
	HostReleaseName     = "host-release"
)

func (c *Controller) Ensure(ctx context.Context, dyn dynamic.Interface) error {
	if err := c.ensureKcpInstallation(ctx, dyn); err != nil {
		return err
	}
	if err := c.ensureHostInstall(ctx, dyn); err != nil {
		return err
	}
	return c.ensureHostRelease(ctx, dyn)
}

func (c *Controller) EnsureAndReconcile(ctx context.Context, dyn dynamic.Interface) error {
	if err := c.Ensure(ctx, dyn); err != nil {
		return err
	}
	items := []struct {
		gvr       schema.GroupVersionResource
		name      string
		reconcile ReconcileFunc
	}{
		{KcpInstallationGVR, KcpInstallationName, c.ReconcileKcpInstallation},
		{HostInstallGVR, HostInstallName, c.ReconcileHostInstall},
		{HostReleaseGVR, HostReleaseName, c.ReconcileHostRelease},
	}
	for _, item := range items {
		u, err := dyn.Resource(item.gvr).Get(ctx, item.name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := item.reconcile(ctx, dyn, u); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) ensureKcpInstallation(ctx context.Context, dyn dynamic.Interface) error {
	ri := dyn.Resource(KcpInstallationGVR)
	_, err := ri.Get(ctx, KcpInstallationName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	obs, oerr := c.authority.ObserveKcpInstallation(ctx, v1alpha1.KcpInstallationSpec{})
	if oerr != nil {
		c.log.Warn("reading the kcp installation authority for creation", "err", oerr.Error())
	}
	spec := map[string]any{}
	if obs.ActiveSlot != "" {
		spec["selectedSlot"] = string(obs.ActiveSlot)
	}
	if obs.FrontBackend != "" {
		spec["frontBackend"] = obs.FrontBackend
	}
	return c.create(ctx, ri, "KcpInstallation", KcpInstallationName, spec)
}

func (c *Controller) ensureHostInstall(ctx context.Context, dyn dynamic.Interface) error {
	ri := dyn.Resource(HostInstallGVR)
	_, err := ri.Get(ctx, HostInstallName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	obs, oerr := c.authority.ObserveHostInstall(ctx, v1alpha1.HostInstallSpec{})
	if oerr != nil {
		c.log.Warn("reading the host install authority for creation", "err", oerr.Error())
	}
	spec := map[string]any{"account": obs.Account, "version": obs.InstalledVersion}
	return c.create(ctx, ri, "HostInstall", HostInstallName, spec)
}

func (c *Controller) ensureHostRelease(ctx context.Context, dyn dynamic.Interface) error {
	ri := dyn.Resource(HostReleaseGVR)
	_, err := ri.Get(ctx, HostReleaseName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	obs, oerr := c.authority.ObserveHostRelease(ctx, v1alpha1.HostReleaseSpec{})
	if oerr != nil {
		c.log.Warn("reading the host release authority for creation", "err", oerr.Error())
	}
	spec := map[string]any{"version": obs.Current}
	return c.create(ctx, ri, "HostRelease", HostReleaseName, spec)
}

func (c *Controller) create(ctx context.Context, ri dynamic.ResourceInterface, kind, name string, spec map[string]any) error {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
	_, err := ri.Create(ctx, u, metav1.CreateOptions{FieldManager: c.fieldManager})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}
