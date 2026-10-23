package appctl

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/unstruct"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const ConditionPdsIngress = "PdsIngress"

const (
	PdsIngressReasonReady               = "Ready"
	PdsIngressReasonNotRequired         = "NotRequired"
	PdsIngressReasonProviderMissing     = "IngressProviderMissing"
	PdsIngressReasonProviderUnsupported = "IngressProviderUnsupported"
	PdsIngressReasonProviderNotReady    = "IngressProviderNotReady"
	PdsIngressReasonNotReady            = "IngressNotReady"
	PdsIngressReasonURLInvalid          = "IngressURLInvalid"
	PdsIngressReasonStableHost          = "StableHostMismatch"
)

type pdsIngressError struct {
	Reason  string
	Message string
}

func (e *pdsIngressError) Error() string {
	return e.Reason + ": " + e.Message
}

func pdsIngressErr(reason, format string, args ...any) error {
	return &pdsIngressError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func pdsIngressReason(err error) string {
	if e, ok := err.(*pdsIngressError); ok {
		return e.Reason
	}
	return PdsIngressReasonNotReady
}

func appIngressConfig(spec *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue) (*v1alpha1.IngressConfig, bool) {
	if spec != nil {
		return spec, false
	}
	if network != nil && network.Ingress != nil {
		return network.Ingress, true
	}
	return nil, false
}

func pdsIngressRef(cfg *v1alpha1.IngressConfig, fromNetwork bool) *commonv1.ObjectRef {
	if cfg != nil && cfg.IngressRef != nil {
		if name := strings.TrimSpace(cfg.IngressRef.Name); name != "" {
			return ComponentRefFor(ComponentKindIngressProxy, name)
		}
	}
	if fromNetwork {
		return ComponentRefFor(ComponentKindIngressProxy, NetworkIngressName)
	}
	return ComponentRefFor(ComponentKindIngressProxy, AppIngressName)
}

func pdsAppIngress(spec *v1alpha1.IngressConfig, network *v1alpha1.MarketNetworkValue, production bool) (bool, *commonv1.ObjectRef, *v1alpha1.IngressConfig, error) {
	cfg, fromNetwork := appIngressConfig(spec, network)
	if IngressProviderFor(cfg) == v1alpha1.IngressProviderIroh {
		return true, nil, nil, pdsIngressErr(PdsIngressReasonProviderUnsupported,
			"the Pds cannot use ingress provider %s until a later plan", v1alpha1.IngressProviderIroh)
	}
	if IngressProviderFor(cfg) == "" {
		if cfg != nil || production {
			return true, nil, nil, pdsIngressErr(PdsIngressReasonProviderMissing,
				"no Pds ingress provider is selected; set spec.ingress.xrpc.ingressProxyHost or spec.ingress.opentunnel")
		}
		return false, nil, nil, nil
	}
	exposure := cfg
	if fromNetwork {
		exposure = nil
	}
	return true, pdsIngressRef(cfg, fromNetwork), exposure, nil
}

func networkIngressSelection(spec *v1alpha1.IngressConfig) error {
	if spec == nil {
		return nil
	}
	switch IngressProviderFor(spec) {
	case v1alpha1.IngressProviderIroh:
		return pdsIngressErr(PdsIngressReasonProviderUnsupported,
			"the network Pds cannot use ingress provider %s until a later plan", v1alpha1.IngressProviderIroh)
	case "":
		if strings.TrimSpace(spec.Provider) != "" {
			return pdsIngressErr(PdsIngressReasonProviderMissing,
				"the network Pds ingress provider %q is not known", spec.Provider)
		}
	}
	return nil
}

func withoutPdsNamed(desired []desiredComponent, names ...string) []desiredComponent {
	out := make([]desiredComponent, 0, len(desired))
	for _, d := range desired {
		if d.Kind == ComponentKindPds && slices.Contains(names, d.Name) {
			continue
		}
		out = append(out, d)
	}
	return out
}

func pdsApplySet(desired []desiredComponent, pdsName, ingressRef string) []desiredComponent {
	prefix := serviceNameFor(ComponentKindPds, pdsName)
	out := make([]desiredComponent, 0, 5)
	for _, d := range desired {
		switch {
		case d.Kind == ComponentKindPds && d.Name == pdsName:
			out = append(out, d)
		case d.Kind == ComponentKindService && d.Name == prefix:
			out = append(out, d)
		case d.Kind == ComponentKindExposure && strings.HasPrefix(d.Name, prefix+"-"):
			out = append(out, d)
		case d.Kind == ComponentKindIngressProxy && ingressRef != "" && d.Name == ingressRef:
			out = append(out, d)
		}
	}
	return out
}

func pdsIngressExposureName(pdsName string) string {
	return serviceNameFor(ComponentKindPds, pdsName) + ExposureIngressSuffix
}

func schemeHostURL(raw string) (string, string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", false
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", false
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host, u.Host, true
}

func (ctl *Controller) pdsIngressURL(ctx context.Context, ws, pdsName string) (string, error) {
	raw := ctl.pdsExposureURLOf(ctx, ws, pdsName, ExposureIngressSuffix)
	if raw == "" {
		return "", pdsIngressErr(PdsIngressReasonNotReady,
			"the ingress Exposure %s reports no public URL yet", pdsIngressExposureName(pdsName))
	}
	base, host, ok := schemeHostURL(raw)
	if !ok {
		return "", pdsIngressErr(PdsIngressReasonURLInvalid,
			"the ingress Exposure %s reports %q, which is not a scheme, host and optional port", pdsIngressExposureName(pdsName), raw)
	}
	stable, err := ctl.pdsStableHost(ctx, ws, pdsName)
	if err != nil {
		return "", err
	}
	if stable != "" && !strings.EqualFold(stable, host) {
		return "", pdsIngressErr(PdsIngressReasonStableHost,
			"the Pds public host was %s and the ingress now returns %s; the DID service endpoint is not rewritten", stable, host)
	}
	return base, nil
}

func (ctl *Controller) pdsPublicURLFor(ctx context.Context, ws, pdsName string, requireIngress bool) (string, error) {
	if requireIngress {
		return ctl.pdsIngressURL(ctx, ws, pdsName)
	}
	if raw := ctl.pdsExposureURLOf(ctx, ws, pdsName, ExposureIngressSuffix); raw != "" {
		if base, _, ok := schemeHostURL(raw); ok {
			return base, nil
		}
	}
	raw := ctl.pdsExposureURLOf(ctx, ws, pdsName, ExposureLocalSuffix)
	if raw == "" {
		return "", nil
	}
	if base, _, ok := schemeHostURL(raw); ok {
		return base, nil
	}
	return strings.TrimRight(strings.TrimSpace(raw), "/"), nil
}

func (ctl *Controller) pdsStableHost(ctx context.Context, ws, pdsName string) (string, error) {
	spec, found, err := ctl.pdsSpecIn(ctx, ws, pdsName)
	if err != nil || !found {
		return "", err
	}
	return strings.TrimSpace(spec.StableHost), nil
}

func (ctl *Controller) pdsSpecIn(ctx context.Context, ws, pdsName string) (v1alpha1.PdsSpec, bool, error) {
	ri, err := ctl.client(ws, PdsGVR)
	if err != nil {
		return v1alpha1.PdsSpec{}, false, err
	}
	u, err := ri.Get(ctx, pdsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return v1alpha1.PdsSpec{}, false, nil
	}
	if err != nil {
		return v1alpha1.PdsSpec{}, false, err
	}
	var o v1alpha1.Pds
	if err := unstruct.Into(u, &o); err != nil {
		return v1alpha1.PdsSpec{}, false, err
	}
	return o.Spec, true, nil
}

func (ctl *Controller) pdsPublicURLApplied(ctx context.Context, ws, pdsName string) (bool, error) {
	spec, found, err := ctl.pdsSpecIn(ctx, ws, pdsName)
	if err != nil || !found {
		return false, err
	}
	return strings.TrimSpace(spec.PublicBaseURL) != "", nil
}

func componentRefName(ref *commonv1.ObjectRef) string {
	if ref == nil {
		return ""
	}
	return strings.TrimSpace(ref.Name)
}

func setPdsIngressCondition(st *v1alpha1.ApplicationStatus, generation int64, requireIngress bool, err error) {
	setPdsIngressConditionNamed(&st.Conditions, generation, "", requireIngress, err)
}

func pdsIngressConditionType(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || name == PdsComponent {
		return ConditionPdsIngress
	}
	return ConditionPdsIngress + strings.ToUpper(name[:1]) + name[1:]
}

func setPdsIngressConditionNamed(conds *[]metav1.Condition, generation int64, name string, requireIngress bool, err error) {
	conditionType := pdsIngressConditionType(name)
	if !requireIngress {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionType, Status: metav1.ConditionTrue,
			Reason: PdsIngressReasonNotRequired, Message: "the Pds does not require an ingress", ObservedGeneration: generation})
		return
	}
	if err == nil {
		meta.SetStatusCondition(conds, metav1.Condition{Type: conditionType, Status: metav1.ConditionTrue,
			Reason: PdsIngressReasonReady, Message: "the Pds ingress is ready", ObservedGeneration: generation})
		return
	}
	meta.SetStatusCondition(conds, metav1.Condition{Type: conditionType, Status: metav1.ConditionFalse,
		Reason: pdsIngressReason(err), Message: err.Error(), ObservedGeneration: generation})
}

func pdsIngressPhase(err error) v1alpha1.ApplicationPhase {
	if pdsIngressReason(err) == PdsIngressReasonStableHost {
		return v1alpha1.ApplicationPhaseFailed
	}
	return v1alpha1.ApplicationPhaseProvisioning
}

func (ctl *Controller) pdsIngressReady(snap componentSnapshot, pdsName string, ref *commonv1.ObjectRef) error {
	if ref != nil {
		if view, ok := snap.view(ComponentKindIngressProxy, ref.Name); ok && !view.Status.Ready {
			return pdsIngressErr(PdsIngressReasonProviderNotReady,
				"the IngressProxy %s is not ready", ref.Name)
		}
	}
	if !snap.ready(ComponentKindExposure, pdsIngressExposureName(pdsName)) {
		return pdsIngressErr(PdsIngressReasonNotReady,
			"the ingress Exposure %s is not ready", pdsIngressExposureName(pdsName))
	}
	return nil
}
