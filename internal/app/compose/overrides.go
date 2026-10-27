package compose

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	ReasonInvalidKey       = "InvalidKey"
	ReasonAmbiguousKey     = "AmbiguousKey"
	ReasonSettingsMismatch = "SettingsMismatch"
	ReasonInvalidValue     = "InvalidValue"
)

type OverrideError struct {
	Key     string
	Reason  string
	Message string
}

func (e *OverrideError) Error() string {
	if e.Key == "" {
		return "components: " + e.Message
	}
	return "components." + e.Key + ": " + e.Message
}

func overrideErr(key, reason, format string, args ...any) error {
	return &OverrideError{Key: key, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func OverrideErrorReason(err error) string {
	if typed, ok := err.(*OverrideError); ok && typed.Reason != "" {
		return typed.Reason
	}
	return ReasonInvalidValue
}

func resolveOverrides(overrides v1alpha1.ComponentOverrides, desired []Desired) (map[string]v1alpha1.ComponentOverride, error) {
	out := make(map[string]v1alpha1.ComponentOverride, len(overrides))
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kind, name, explicit := strings.Cut(key, "/")
		if strings.TrimSpace(key) == "" || strings.TrimSpace(kind) == "" || (explicit && strings.TrimSpace(name) == "") {
			return nil, overrideErr(key, ReasonInvalidKey, "a key names a component kind or a kind and a name")
		}
		var matches []Desired
		for _, d := range desired {
			if IsExposureKind(d.Kind) || d.Kind != kind || (explicit && d.Name != name) {
				continue
			}
			matches = append(matches, d)
		}
		if len(matches) == 0 {
			return nil, overrideErr(key, ReasonInvalidKey, "no %s component is in this root's desired set", key)
		}
		if !explicit && len(matches) > 1 {
			return nil, overrideErr(key, ReasonAmbiguousKey, "%s names %d components; use %s/%s", kind, len(matches), kind, matches[0].Name)
		}
		ov := overrides[key]
		if err := settingsFor(key, matches[0].Kind, ov.Settings); err != nil {
			return nil, err
		}
		out[ComponentKey(matches[0].Kind, matches[0].Name)] = ov
	}
	return out, nil
}

func settingsBlocks(settings *v1alpha1.ComponentSettings) (map[string]any, error) {
	if settings == nil {
		return nil, nil
	}
	var out map[string]any
	if err := jsonCopy(settings, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func settingsFor(key, kind string, settings *v1alpha1.ComponentSettings) error {
	blocks, err := settingsBlocks(settings)
	if err != nil {
		return overrideErr(key, ReasonInvalidValue, "the settings do not encode: %v", err)
	}
	want := lowerFirst(kind)
	for name := range blocks {
		if name != want {
			return overrideErr(key, ReasonSettingsMismatch, "settings.%s does not configure a %s component", name, kind)
		}
	}
	return nil
}

func ApplyOverrides(overrides v1alpha1.ComponentOverrides, desired []Desired) ([]Desired, error) {
	resolved, err := resolveOverrides(overrides, desired)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return desired, nil
	}
	out := make([]Desired, len(desired))
	copy(out, desired)
	for i := range out {
		d := &out[i]
		if IsExposureKind(d.Kind) {
			continue
		}
		ov, ok := resolved[ComponentKey(d.Kind, d.Name)]
		if !ok {
			continue
		}
		spec, err := overrideSpec(d.Kind, d.Spec, ov)
		if err != nil {
			return nil, err
		}
		d.Spec = spec
	}
	return out, nil
}

func overrideSpec(kind string, spec any, ov v1alpha1.ComponentOverride) (map[string]any, error) {
	base, err := SpecMap(spec)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if base != nil {
		if err := jsonCopy(base, &out); err != nil {
			return nil, err
		}
	}
	if ov.Suspend != nil && *ov.Suspend {
		out[suspendField(kind)] = true
	}
	if ov.RestartPolicy != "" {
		out["restartPolicy"] = string(ov.RestartPolicy)
	}
	if err := setJSON(out, "limits", ov.Limits); err != nil {
		return nil, err
	}
	if err := setJSON(out, "serviceAuth", ov.ServiceAuth); err != nil {
		return nil, err
	}
	if err := setJSON(out, "comms", ov.Comms); err != nil {
		return nil, err
	}
	if ov.Ingress != nil {
		if err := ingressOverride(kind, out, ov.Ingress); err != nil {
			return nil, err
		}
	}
	blocks, err := settingsBlocks(ov.Settings)
	if err != nil {
		return nil, err
	}
	if block, ok := blocks[lowerFirst(kind)].(map[string]any); ok {
		deepMerge(out, block)
	}
	return out, nil
}

func suspendField(kind string) string {
	switch kind {
	case ComponentKindBidder, ComponentKindRequester:
		return "suspendParticipation"
	}
	return "suspend"
}

func OverrideComms(overrides v1alpha1.ComponentOverrides, kind, name string, base *v1alpha1.MarketComms) *v1alpha1.MarketComms {
	for _, key := range []string{ComponentKey(kind, name), kind} {
		if ov, ok := overrides[key]; ok && ov.Comms != nil {
			return ov.Comms
		}
	}
	return base
}

func ingressOverride(kind string, out map[string]any, ingress *v1alpha1.IngressConfig) error {
	switch kind {
	case ComponentKindRequester:
		return setJSON(out, "ingress", ingress)
	case ComponentKindPds:
		if ingress.IngressRef != nil {
			return setJSON(out, "ingressRef", ingress.IngressRef)
		}
	case ComponentKindIngressProxy:
		if p := strings.TrimSpace(ingress.Provider); p != "" {
			out["provider"] = p
		}
		if host := strings.TrimSpace(ingress.PublicHost); host != "" {
			out["publicHost"] = host
			out["hostnames"] = []any{IngressHostname(host)}
			identity, _ := out["identity"].(map[string]any)
			if identity == nil {
				identity = map[string]any{}
			}
			identity["handle"] = IngressHostname(host)
			out["identity"] = identity
		}
		if x := ingress.Xrpc; x != nil {
			if host := strings.TrimSpace(x.IngressProxyHost); host != "" {
				out["ingressProxyHost"] = host
			}
			if relay := strings.TrimSpace(x.RelayURL); relay != "" {
				out["relayURL"] = relay
			}
			if err := setJSON(out, "relayRef", x.RelayRef); err != nil {
				return err
			}
			if err := setJSON(out, "identityRef", x.IdentityRef); err != nil {
				return err
			}
		}
		return setJSON(out, "opentunnel", ingress.OpenTunnel)
	}
	return nil
}

func setJSON(out map[string]any, key string, value any) error {
	if isNil(value) {
		return nil
	}
	var v any
	if err := jsonCopy(value, &v); err != nil {
		return err
	}
	out[key] = v
	return nil
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if v == nil {
			continue
		}
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
