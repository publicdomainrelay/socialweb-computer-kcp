package appctl

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

const (
	ConditionComponentsReady         = "ComponentsReady"
	ConditionComponentSuspended      = "ComponentSuspended"
	ConditionComponentOverridesValid = "ComponentOverridesValid"
	ConditionComponentDrift          = "ComponentDrift"
	ConditionLimitsEnforced          = "LimitsEnforced"

	ReasonInvalidKey              = "InvalidKey"
	ReasonAmbiguousKey            = "AmbiguousKey"
	ReasonSettingsMismatch        = "SettingsMismatch"
	ReasonUnsupportedOverride     = "UnsupportedOverride"
	ReasonInvalidValue            = "InvalidValue"
	ReasonOverridesApplied        = "OverridesApplied"
	ReasonNoOverrides             = "NoOverrides"
	ReasonSuspended               = "Suspended"
	ReasonNotSuspended            = "NotSuspended"
	ReasonChildSpecDrift          = "ChildSpecDrift"
	ReasonNoChildSpecDrift        = "NoChildSpecDrift"
	ReasonComponentLimitsInactive = "ComponentLimitsNotEnforced"
)

type overrideError struct {
	Key     string
	Reason  string
	Message string
}

func (e *overrideError) Error() string {
	if e.Key == "" {
		return "components: " + e.Message
	}
	return "components." + e.Key + ": " + e.Message
}

func newOverrideError(key, reason, format string, args ...any) error {
	return &overrideError{Key: key, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func overrideErrorReason(err error) string {
	if typed, ok := err.(*overrideError); ok && typed.Reason != "" {
		return typed.Reason
	}
	return ReasonInvalidValue
}

func overrideErrorKey(err error) string {
	if typed, ok := err.(*overrideError); ok {
		return typed.Key
	}
	return ""
}

func settingsKindFor(kind string) string {
	switch kind {
	case ComponentKindBidder:
		return "bidder"
	case ComponentKindRequester:
		return "requester"
	case ComponentKindGuestControlPlane:
		return "guestControlPlane"
	case ComponentKindPds:
		return "pds"
	case ComponentKindSshServer:
		return "sshServer"
	case ComponentKindGatewayAPI:
		return "gatewayAPI"
	case ComponentKindIngressProxy:
		return "ingressProxy"
	case ComponentKindRelay:
		return "relay"
	case ComponentKindPolicyEngine:
		return "policyEngine"
	}
	return ""
}

func settingsBlocks(settings *v1alpha1.ComponentSettings) []string {
	if settings == nil {
		return nil
	}
	var out []string
	if settings.Bidder != nil {
		out = append(out, "bidder")
	}
	if settings.Requester != nil {
		out = append(out, "requester")
	}
	if settings.GuestControlPlane != nil {
		out = append(out, "guestControlPlane")
	}
	if settings.Pds != nil {
		out = append(out, "pds")
	}
	if settings.SshServer != nil {
		out = append(out, "sshServer")
	}
	if settings.GatewayAPI != nil {
		out = append(out, "gatewayAPI")
	}
	if settings.IngressProxy != nil {
		out = append(out, "ingressProxy")
	}
	if settings.Relay != nil {
		out = append(out, "relay")
	}
	if settings.PolicyEngine != nil {
		out = append(out, "policyEngine")
	}
	return out
}

func validateComponentOverride(key, kind string, ov v1alpha1.ComponentOverride) error {
	if blocks := settingsBlocks(ov.Settings); len(blocks) > 0 {
		want := settingsKindFor(kind)
		if want == "" {
			return newOverrideError(key, ReasonSettingsMismatch, "%s has no settings block", kind)
		}
		if len(blocks) != 1 || blocks[0] != want {
			return newOverrideError(key, ReasonSettingsMismatch, "settings must hold exactly the %s block for a %s component, got %s",
				want, kind, strings.Join(blocks, ", "))
		}
	}
	if ov.Comms != nil && kind != ComponentKindBidder && kind != ComponentKindRequester {
		return newOverrideError(key, ReasonUnsupportedOverride, "comms is not a field of %s", kind)
	}
	if ov.Ingress != nil && kind != ComponentKindPds && kind != ComponentKindIngressProxy {
		return newOverrideError(key, ReasonUnsupportedOverride, "ingress is not a field of %s", kind)
	}
	if ov.RestartPolicy != "" {
		switch ov.RestartPolicy {
		case v1alpha1.ComponentRestartNever, v1alpha1.ComponentRestartOnFailure, v1alpha1.ComponentRestartAlways:
		default:
			return newOverrideError(key, ReasonInvalidValue, "unknown restartPolicy %q", ov.RestartPolicy)
		}
		if kind == ComponentKindPolicyEngine {
			return newOverrideError(key, ReasonUnsupportedOverride, "restartPolicy is not a field of %s", kind)
		}
	}
	if ov.Limits != nil {
		if kind == ComponentKindPolicyEngine {
			return newOverrideError(key, ReasonUnsupportedOverride, "limits is not a field of %s; use settings.policyEngine.limits", kind)
		}
		if ov.Limits.CpuMillicores < 0 || ov.Limits.MemoryBytes < 0 || ov.Limits.DiskBytes < 0 {
			return newOverrideError(key, ReasonInvalidValue, "limits must not be negative")
		}
	}
	if ov.ServiceAuth != nil && kind == ComponentKindPolicyEngine {
		return newOverrideError(key, ReasonUnsupportedOverride, "serviceAuth is not a field of %s", kind)
	}
	return validateSettings(key, kind, ov.Settings)
}

func validateSeconds(key, name string, value *int32) error {
	if value != nil && *value < 0 {
		return newOverrideError(key, ReasonInvalidValue, "%s must not be negative", name)
	}
	return nil
}

func validateMilliseconds(key, name string, value *int32) error {
	return validateSeconds(key, name, value)
}

func validateCount(key, name string, value *int32) error {
	if value != nil && *value < 0 {
		return newOverrideError(key, ReasonInvalidValue, "%s must not be negative", name)
	}
	return nil
}

func validateBytes(key, name string, value *int64) error {
	if value != nil && *value < 0 {
		return newOverrideError(key, ReasonInvalidValue, "%s must not be negative", name)
	}
	return nil
}

func validateSettings(key, kind string, settings *v1alpha1.ComponentSettings) error {
	if settings == nil {
		return nil
	}
	switch kind {
	case ComponentKindBidder:
		if s := settings.Bidder; s != nil {
			for name, value := range map[string]*int32{
				"heartbeatIntervalSeconds": s.HeartbeatIntervalSeconds,
				"offeringRefreshSeconds":   s.OfferingRefreshSeconds,
				"bidTtlSeconds":            s.BidTTLSeconds,
				"maxConcurrentGuests":      s.MaxConcurrentGuests,
				"policyPollMilliseconds":   s.PolicyPollMilliseconds,
				"policyTimeoutSeconds":     s.PolicyTimeoutSeconds,
			} {
				if err := validateCount(key, name, value); err != nil {
					return err
				}
			}
		}
	case ComponentKindRequester:
		if s := settings.Requester; s != nil {
			for name, value := range map[string]*int32{
				"maxConcurrent":            s.MaxConcurrent,
				"perAccount":               s.PerAccount,
				"contractPollMilliseconds": s.ContractPollMilliseconds,
				"bidWindowSeconds":         s.BidWindowSeconds,
				"receiptTimeoutSeconds":    s.ReceiptTimeoutSeconds,
				"receiptPollMilliseconds":  s.ReceiptPollMilliseconds,
				"sessionCapSeconds":        s.SessionCapSeconds,
				"sessionIdleSeconds":       s.SessionIdleSeconds,
				"directoryTTLSeconds":      s.DirectoryTTLSeconds,
				"directoryKeys":            s.DirectoryKeys,
				"policyPollMilliseconds":   s.PolicyPollMilliseconds,
				"policyTimeoutSeconds":     s.PolicyTimeoutSeconds,
			} {
				if err := validateCount(key, name, value); err != nil {
					return err
				}
			}
		}
	case ComponentKindGuestControlPlane:
		if s := settings.GuestControlPlane; s != nil {
			for name, value := range map[string]*int32{
				"grantSyncSeconds":   s.GrantSyncSeconds,
				"issuerRetrySeconds": s.IssuerRetrySeconds,
				"hostKeyAttempts":    s.HostKeyAttempts,
			} {
				if err := validateCount(key, name, value); err != nil {
					return err
				}
			}
		}
	case ComponentKindPds:
		if s := settings.Pds; s != nil {
			if err := validateBytes(key, "maxBlobSize", s.MaxBlobSize); err != nil {
				return err
			}
			if err := validateBytes(key, "maxRequestBytes", s.MaxRequestBytes); err != nil {
				return err
			}
			if s.OAuth != nil {
				for name, value := range map[string]*int32{
					"accessTokenTTLSeconds":  s.OAuth.AccessTokenTTLSeconds,
					"refreshTokenTTLSeconds": s.OAuth.RefreshTokenTTLSeconds,
				} {
					if err := validateCount(key, name, value); err != nil {
						return err
					}
				}
				if len(s.OAuth.Scopes) > 0 && !slices.ContainsFunc(s.OAuth.Scopes, func(scope string) bool { return strings.TrimSpace(scope) == "atproto" }) {
					return newOverrideError(key, ReasonInvalidValue, "oauth scopes must include atproto")
				}
			}
		}
	case ComponentKindSshServer:
		if s := settings.SshServer; s != nil {
			for name, value := range map[string]*int32{
				"maxConnections":     s.MaxConnections,
				"maxSessions":        s.MaxSessions,
				"sessionsPerAccount": s.SessionsPerAccount,
				"maxAuthAttempts":    s.MaxAuthAttempts,
				"authTimeoutSeconds": s.AuthTimeoutSeconds,
				"sessionMaxSeconds":  s.SessionMaxSeconds,
				"cacheEntries":       s.CacheEntries,
			} {
				if err := validateCount(key, name, value); err != nil {
					return err
				}
			}
		}
	case ComponentKindGatewayAPI:
		if s := settings.GatewayAPI; s != nil {
			if err := validateCount(key, "nodePollIntervalSeconds", s.NodePollIntervalSeconds); err != nil {
				return err
			}
		}
	case ComponentKindIngressProxy:
		if s := settings.IngressProxy; s != nil {
			for name, value := range map[string]*int32{
				"relayTimeoutMilliseconds":     s.RelayTimeoutMilliseconds,
				"keepaliveMilliseconds":        s.KeepaliveMilliseconds,
				"keepaliveTimeoutMilliseconds": s.KeepaliveTimeoutMilliseconds,
				"reconnectGraceMilliseconds":   s.ReconnectGraceMilliseconds,
				"nonceTTLMilliseconds":         s.NonceTTLMilliseconds,
			} {
				if err := validateCount(key, name, value); err != nil {
					return err
				}
			}
		}
	case ComponentKindRelay:
		if s := settings.Relay; s != nil {
			if err := validateCount(key, "crawlIntervalSeconds", s.CrawlIntervalSeconds); err != nil {
				return err
			}
		}
	case ComponentKindPolicyEngine:
		if s := settings.PolicyEngine; s != nil {
			if s.Limits != nil {
				for name, value := range map[string]*int32{
					"maxConcurrentRuns": s.Limits.MaxConcurrentRuns,
					"queueDepth":        s.Limits.QueueDepth,
					"maxCacheEntries":   s.Limits.MaxCacheEntries,
				} {
					if err := validateCount(key, name, value); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func resolveComponentOverrides(overrides v1alpha1.ComponentOverrides, desired []desiredComponent) (map[string]v1alpha1.ComponentOverride, error) {
	out := make(map[string]v1alpha1.ComponentOverride, len(overrides))
	if len(overrides) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		kind, name, explicit := strings.Cut(key, "/")
		if strings.TrimSpace(key) == "" || strings.TrimSpace(kind) == "" || (explicit && strings.TrimSpace(name) == "") {
			return nil, newOverrideError(key, ReasonInvalidKey, "a key names a component kind or a kind and a name")
		}
		var matches []desiredComponent
		for _, d := range desired {
			if isExposureKind(d.Kind) || d.Kind != kind {
				continue
			}
			if explicit && d.Name != name {
				continue
			}
			matches = append(matches, d)
		}
		if len(matches) == 0 {
			return nil, newOverrideError(key, ReasonInvalidKey, "no %s component is in this root's desired set", key)
		}
		if !explicit && len(matches) > 1 {
			return nil, newOverrideError(key, ReasonAmbiguousKey, "%s names %d components; use %s/%s", kind, len(matches), kind, matches[0].Name)
		}
		match := matches[0]
		ov := overrides[key]
		if err := validateComponentOverride(key, match.Kind, ov); err != nil {
			return nil, err
		}
		out[ComponentKey(match.Kind, match.Name)] = ov
	}
	return out, nil
}

func applyComponentOverrides(rootSuspend bool, overrides v1alpha1.ComponentOverrides, desired []desiredComponent) ([]desiredComponent, error) {
	resolved, err := resolveComponentOverrides(overrides, desired)
	if err != nil {
		return nil, err
	}
	if len(resolved) == 0 {
		return desired, nil
	}
	out := make([]desiredComponent, len(desired))
	copy(out, desired)
	for i := range out {
		d := &out[i]
		if isExposureKind(d.Kind) {
			continue
		}
		ov, ok := resolved[ComponentKey(d.Kind, d.Name)]
		if !ok {
			continue
		}
		spec, err := overrideComponentSpec(d.Kind, d.Spec, ov, rootSuspend)
		if err != nil {
			return nil, err
		}
		d.Spec = spec
	}
	return out, nil
}

func applyCommonOverride(cs *v1alpha1.ComponentSpec, ov v1alpha1.ComponentOverride, rootSuspend bool) {
	if ov.Suspend != nil {
		cs.Suspend = rootSuspend || *ov.Suspend
	}
	if ov.RestartPolicy != "" {
		cs.RestartPolicy = ov.RestartPolicy
	}
	if ov.Limits != nil {
		limits := *ov.Limits
		cs.Limits = &limits
	}
	if ov.ServiceAuth != nil {
		cs.ServiceAuth = &v1alpha1.ComponentServiceAuth{
			Audience: append([]string(nil), ov.ServiceAuth.Audience...),
			Hostname: strings.TrimSpace(ov.ServiceAuth.Hostname),
		}
	}
}

func seconds(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func milliseconds(value *int32) int32 {
	if value == nil {
		return 0
	}
	return *value
}

func overrideComponentSpec(kind string, spec any, ov v1alpha1.ComponentOverride, rootSuspend bool) (any, error) {
	switch typed := spec.(type) {
	case v1alpha1.BidderSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.Bidder != nil {
			s := settings.Bidder
			typed.HeartbeatIntervalSeconds = s.HeartbeatIntervalSeconds
			typed.OfferingRefreshSeconds = s.OfferingRefreshSeconds
			typed.BidTTLSeconds = s.BidTTLSeconds
			if s.MaxConcurrentGuests != nil {
				typed.MaxConcurrentGuests = *s.MaxConcurrentGuests
			}
			typed.PolicyPollMilliseconds = s.PolicyPollMilliseconds
			typed.PolicyTimeoutSeconds = s.PolicyTimeoutSeconds
		}
		if ov.Comms != nil {
			typed.Comms = ov.Comms
		}
		return typed, nil
	case v1alpha1.RequesterSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.Requester != nil {
			s := settings.Requester
			if s.MaxConcurrent != nil {
				typed.MaxConcurrent = *s.MaxConcurrent
			}
			if s.PerAccount != nil {
				typed.PerAccount = *s.PerAccount
			}
			typed.ContractPollMilliseconds = s.ContractPollMilliseconds
			typed.BidWindowSeconds = s.BidWindowSeconds
			typed.ReceiptTimeoutSeconds = s.ReceiptTimeoutSeconds
			typed.ReceiptPollMilliseconds = s.ReceiptPollMilliseconds
			typed.SessionCapSeconds = s.SessionCapSeconds
			typed.SessionIdleSeconds = s.SessionIdleSeconds
			typed.DirectoryTTLSeconds = s.DirectoryTTLSeconds
			typed.DirectoryKeys = s.DirectoryKeys
			typed.PolicyPollMilliseconds = s.PolicyPollMilliseconds
			typed.PolicyTimeoutSeconds = s.PolicyTimeoutSeconds
			typed.OAuthScopes = append([]string(nil), s.OAuthScopes...)
		}
		if ov.Comms != nil {
			typed.Comms = ov.Comms
		}
		return typed, nil
	case v1alpha1.GuestControlPlaneSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.GuestControlPlane != nil {
			s := settings.GuestControlPlane
			if s.GuestLoginMount != "" {
				typed.GuestLoginMount = s.GuestLoginMount
			}
			if s.Role != "" {
				typed.Role = s.Role
			}
			typed.GrantSyncSeconds = s.GrantSyncSeconds
			typed.IssuerRetrySeconds = s.IssuerRetrySeconds
			typed.HostKeyAttempts = s.HostKeyAttempts
			typed.RelayEnabled = s.RelayEnabled
			if s.HostKeyRef != "" {
				typed.HostKeyRef = s.HostKeyRef
			}
		}
		return typed, nil
	case v1alpha1.PdsSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.Pds != nil {
			s := settings.Pds
			typed.MaxBlobSize = s.MaxBlobSize
			typed.MaxRequestBytes = s.MaxRequestBytes
			if s.ContactEmail != "" {
				typed.ContactEmail = s.ContactEmail
			}
			if s.OAuth != nil {
				oauth := v1alpha1.PdsOAuth{}
				if typed.OAuth != nil {
					oauth = *typed.OAuth
				}
				if s.OAuth.AllowLocalhostClients != nil {
					oauth.AllowLocalhostClients = *s.OAuth.AllowLocalhostClients
				}
				if s.OAuth.AccessTokenTTLSeconds != nil {
					oauth.AccessTokenTTLSeconds = *s.OAuth.AccessTokenTTLSeconds
				}
				if s.OAuth.RefreshTokenTTLSeconds != nil {
					oauth.RefreshTokenTTLSeconds = *s.OAuth.RefreshTokenTTLSeconds
				}
				if s.OAuth.DisableLocalhostClients != nil {
					oauth.DisableLocalhostClients = *s.OAuth.DisableLocalhostClients
				}
				if len(s.OAuth.Scopes) > 0 {
					oauth.Scopes = append([]string(nil), s.OAuth.Scopes...)
				}
				typed.OAuth = &oauth
			}
		}
		if ov.Ingress != nil && ov.Ingress.IngressRef != nil {
			ref := *ov.Ingress.IngressRef
			typed.IngressRef = &ref
		}
		return typed, nil
	case v1alpha1.SshServerSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.SshServer != nil {
			s := settings.SshServer
			if s.MaxConnections != nil {
				typed.MaxConnections = *s.MaxConnections
			}
			if s.MaxSessions != nil {
				typed.MaxSessions = *s.MaxSessions
			}
			if s.SessionsPerAccount != nil {
				typed.SessionsPerAccount = *s.SessionsPerAccount
			}
			if s.MaxAuthAttempts != nil {
				typed.MaxAuthAttempts = *s.MaxAuthAttempts
			}
			typed.AuthTimeoutSeconds = s.AuthTimeoutSeconds
			typed.SessionMaxSeconds = s.SessionMaxSeconds
			if s.CacheEntries != nil {
				typed.CacheEntries = *s.CacheEntries
			}
		}
		return typed, nil
	case v1alpha1.GatewayAPISpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.GatewayAPI != nil {
			s := settings.GatewayAPI
			typed.NodePollIntervalSeconds = s.NodePollIntervalSeconds
			if len(s.RequiredDownloads) > 0 {
				typed.RequiredDownloads = append([]string(nil), s.RequiredDownloads...)
			}
		}
		return typed, nil
	case v1alpha1.IngressProxySpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.IngressProxy != nil {
			s := settings.IngressProxy
			typed.RelayTimeoutMilliseconds = s.RelayTimeoutMilliseconds
			typed.KeepaliveMilliseconds = s.KeepaliveMilliseconds
			typed.KeepaliveTimeoutMilliseconds = s.KeepaliveTimeoutMilliseconds
			typed.ReconnectGraceMilliseconds = s.ReconnectGraceMilliseconds
			typed.NonceTTLMilliseconds = s.NonceTTLMilliseconds
		}
		if ingress := ov.Ingress; ingress != nil {
			if provider := strings.TrimSpace(ingress.Provider); provider != "" {
				typed.Provider = provider
			}
			if host := strings.TrimSpace(ingress.PublicHost); host != "" {
				typed.PublicHost = host
				typed.Hostnames = []string{ingressHostname(host)}
				if typed.Identity == nil {
					typed.Identity = &v1alpha1.ComponentIdentity{}
				}
				typed.Identity.Handle = ingressHostname(host)
			}
			if ingress.Xrpc != nil {
				if host := strings.TrimSpace(ingress.Xrpc.IngressProxyHost); host != "" {
					typed.IngressProxyHost = host
				}
				if relay := strings.TrimSpace(ingress.Xrpc.RelayURL); relay != "" {
					typed.RelayURL = relay
				}
				if ingress.Xrpc.RelayRef != nil {
					ref := *ingress.Xrpc.RelayRef
					typed.RelayRef = &ref
				}
				if ingress.Xrpc.IdentityRef != nil {
					ref := *ingress.Xrpc.IdentityRef
					typed.IdentityRef = &ref
				}
			}
			if ingress.OpenTunnel != nil {
				typed.OpenTunnel = &v1alpha1.IngressProxyOpenTunnel{
					Command:       append([]string(nil), ingress.OpenTunnel.Command...),
					RouteName:     strings.TrimSpace(ingress.OpenTunnel.RouteName),
					ProxyProtocol: strings.TrimSpace(ingress.OpenTunnel.ProxyProtocol),
					LocalHost:     strings.TrimSpace(ingress.OpenTunnel.LocalHost),
				}
			}
		}
		return typed, nil
	case v1alpha1.RelaySpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		if settings := ov.Settings; settings != nil && settings.Relay != nil {
			typed.CrawlIntervalSeconds = settings.Relay.CrawlIntervalSeconds
		}
		return typed, nil
	case v1alpha1.PlcSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		return typed, nil
	case v1alpha1.DispatcherTLSSpec:
		applyCommonOverride(&typed.ComponentSpec, ov, rootSuspend)
		return typed, nil
	case policyv1alpha1.PolicyEngineSpec:
		if ov.Suspend != nil {
			typed.Suspend = rootSuspend || *ov.Suspend
		}
		if settings := ov.Settings; settings != nil && settings.PolicyEngine != nil {
			s := settings.PolicyEngine
			if s.Limits != nil {
				if s.Limits.MaxConcurrentRuns != nil {
					typed.Limits.MaxConcurrentRuns = *s.Limits.MaxConcurrentRuns
				}
				if s.Limits.QueueDepth != nil {
					typed.Limits.QueueDepth = *s.Limits.QueueDepth
				}
				typed.Limits.ActiveDeadlineSeconds = s.Limits.ActiveDeadlineSeconds
				if s.Limits.MaxConsoleBytes != nil {
					typed.Limits.MaxConsoleBytes = *s.Limits.MaxConsoleBytes
				}
				if s.Limits.MaxOutputBytes != nil {
					typed.Limits.MaxOutputBytes = *s.Limits.MaxOutputBytes
				}
				if s.Limits.MaxCacheEntries != nil {
					typed.Limits.MaxCacheEntries = *s.Limits.MaxCacheEntries
				}
				if s.Limits.MaxCacheBytes != nil {
					typed.Limits.MaxCacheBytes = *s.Limits.MaxCacheBytes
				}
			}
			if s.Trust != nil {
				typed.Trust.TTLSeconds = s.Trust.TTLSeconds
			}
			if s.Cache != nil {
				typed.Cache.VerdictTTLSeconds = s.Cache.VerdictTTLSeconds
				typed.Cache.ScopeTTLSeconds = s.Cache.ScopeTTLSeconds
				typed.Cache.RecordTTLSeconds = s.Cache.RecordTTLSeconds
			}
		}
		return typed, nil
	}
	return nil, newOverrideError("", ReasonUnsupportedOverride, "%s has no overrideable spec", kind)
}

func desiredSuspended(d desiredComponent) bool {
	switch spec := d.Spec.(type) {
	case v1alpha1.BidderSpec:
		return spec.Suspend
	case v1alpha1.RequesterSpec:
		return spec.Suspend
	case v1alpha1.GuestControlPlaneSpec:
		return spec.Suspend
	case v1alpha1.PdsSpec:
		return spec.Suspend
	case v1alpha1.SshServerSpec:
		return spec.Suspend
	case v1alpha1.GatewayAPISpec:
		return spec.Suspend
	case v1alpha1.IngressProxySpec:
		return spec.Suspend
	case v1alpha1.RelaySpec:
		return spec.Suspend
	case v1alpha1.PlcSpec:
		return spec.Suspend
	case v1alpha1.DispatcherTLSSpec:
		return spec.Suspend
	case policyv1alpha1.PolicyEngineSpec:
		return spec.Suspend
	}
	return false
}

func (s componentSnapshot) satisfied(desired []desiredComponent) bool {
	if len(desired) == 0 {
		return false
	}
	for _, d := range desired {
		if isExposureKind(d.Kind) {
			continue
		}
		v, ok := s.view(d.Kind, d.Name)
		if !ok {
			return false
		}
		if desiredSuspended(d) {
			if v.Status.Phase != v1alpha1.ComponentPhaseSuspended {
				return false
			}
			continue
		}
		if !v.Status.Ready {
			return false
		}
	}
	return true
}

func setOverrideFailure(st *v1alpha1.ApplicationStatus, gen int64, err error) {
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionFalse,
		Reason: overrideErrorReason(err), Message: err.Error(), ObservedGeneration: gen})
	st.ComponentSummary = &v1alpha1.ComponentSummary{}
}

func setOverrideErrorIfAny(st *v1alpha1.ApplicationStatus, gen int64, err error) {
	if _, ok := err.(*overrideError); ok {
		setOverrideFailure(st, gen, err)
	}
}

func fillComponentStatus(st *v1alpha1.ApplicationStatus, gen int64, rootSuspend bool, overrides v1alpha1.ComponentOverrides,
	desired []desiredComponent, snap componentSnapshot, overrideErr error, drift []string) {
	fillComponentStatusFields(&st.Conditions, &st.ComponentSummary, &st.ComponentOverrides, gen, rootSuspend, overrides, desired, snap, overrideErr, drift)
}

func fillNetworkComponentStatus(st *v1alpha1.MarketNetworkStatus, gen int64, rootSuspend bool, overrides v1alpha1.ComponentOverrides,
	desired []desiredComponent, snap componentSnapshot, overrideErr error, drift []string) {
	fillComponentStatusFields(&st.Conditions, &st.ComponentSummary, &st.ComponentOverrides, gen, rootSuspend, overrides, desired, snap, overrideErr, drift)
}

func setNetworkOverrideFailure(st *v1alpha1.MarketNetworkStatus, gen int64, err error) {
	if _, ok := err.(*overrideError); ok {
		meta.SetStatusCondition(&st.Conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionFalse,
			Reason: overrideErrorReason(err), Message: err.Error(), ObservedGeneration: gen})
		st.ComponentSummary = &v1alpha1.ComponentSummary{}
	}
}

func fillComponentStatusFields(conditions *[]metav1.Condition, summaryOut **v1alpha1.ComponentSummary,
	overrideStatusOut *[]v1alpha1.ComponentOverrideStatus, gen int64, rootSuspend bool, overrides v1alpha1.ComponentOverrides,
	desired []desiredComponent, snap componentSnapshot, overrideErr error, drift []string) {
	summary := &v1alpha1.ComponentSummary{}
	var suspended, notReady []string
	for _, d := range desired {
		if isExposureKind(d.Kind) {
			continue
		}
		summary.Desired++
		key := ComponentKey(d.Kind, d.Name)
		v, ok := snap.view(d.Kind, d.Name)
		if !ok {
			summary.NotReady++
			notReady = append(notReady, key)
			continue
		}
		if desiredSuspended(d) {
			summary.Suspended++
			suspended = append(suspended, key)
			continue
		}
		if v.Status.Ready {
			summary.Ready++
		} else {
			summary.NotReady++
			notReady = append(notReady, key)
		}
	}
	*summaryOut = summary
	if summary.NotReady == 0 && summary.Desired > 0 {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentsReady, Status: metav1.ConditionTrue,
			Reason: "Ready", Message: fmt.Sprintf("%d component(s) are ready or suspended by intent", summary.Desired), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentsReady, Status: metav1.ConditionFalse,
			Reason: "Waiting", Message: strings.Join(notReady, "; "), ObservedGeneration: gen})
	}
	if len(suspended) > 0 {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentSuspended, Status: metav1.ConditionTrue,
			Reason: ReasonSuspended, Message: strings.Join(suspended, "; "), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentSuspended, Status: metav1.ConditionFalse,
			Reason: ReasonNotSuspended, Message: "no component is suspended by intent", ObservedGeneration: gen})
	}
	if overrideErr != nil {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionFalse,
			Reason: overrideErrorReason(overrideErr), Message: overrideErr.Error(), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentOverridesValid, Status: metav1.ConditionTrue,
			Reason: ReasonOverridesApplied, Message: fmt.Sprintf("%d component override(s) resolved", len(overrides)), ObservedGeneration: gen})
	}
	if len(drift) > 0 {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentDrift, Status: metav1.ConditionTrue,
			Reason: ReasonChildSpecDrift, Message: strings.Join(drift, "; "), ObservedGeneration: gen})
	} else {
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionComponentDrift, Status: metav1.ConditionFalse,
			Reason: ReasonNoChildSpecDrift, Message: "every child spec matches the root's desired spec", ObservedGeneration: gen})
	}
	var limited []string
	for key, ov := range overrides {
		if ov.Limits != nil {
			limited = append(limited, key)
		}
	}
	if len(limited) > 0 {
		sort.Strings(limited)
		meta.SetStatusCondition(conditions, metav1.Condition{Type: ConditionLimitsEnforced, Status: metav1.ConditionFalse,
			Reason: ReasonComponentLimitsInactive, Message: "the in-process component runtime does not enforce ComponentLimits for " + strings.Join(limited, "; "),
			ObservedGeneration: gen})
	}
	resolved, err := resolveComponentOverrides(overrides, desired)
	if err == nil {
		keys := make([]string, 0, len(resolved))
		for key := range resolved {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]v1alpha1.ComponentOverrideStatus, 0, len(keys))
		for _, key := range keys {
			ov := resolved[key]
			kind, name, _ := strings.Cut(key, "/")
			item := v1alpha1.ComponentOverrideStatus{Component: key, RestartPolicy: string(ov.RestartPolicy),
				Suspend: rootSuspend || (ov.Suspend != nil && *ov.Suspend)}
			if v, ok := snap.view(kind, name); ok {
				item.Generation = v.Generation
				item.Applied = true
			}
			out = append(out, item)
		}
		*overrideStatusOut = out
	}
}
