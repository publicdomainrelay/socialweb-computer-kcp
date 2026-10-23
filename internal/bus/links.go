package bus

import (
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"

	"k8s.io/apimachinery/pkg/labels"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	CapabilityXRPC = "xrpc.call"

	CapabilityStream = "stream.dial"

	ScopeAll = "*"
)

var ErrLinkDenied = errors.New("bus: no accepted ComponentLink allows this call")

type Capability struct {
	Name  string
	Scope []string
}

func (c Capability) String() string {
	if len(c.Scope) == 0 {
		return c.Name
	}
	return c.Name + "{" + strings.Join(c.Scope, ",") + "}"
}

type LinkFrom struct {
	Ref *commonv1.ObjectRef

	Group string

	Kind string

	Workspace string

	Selector labels.Selector
}

type Link struct {
	Workspace string

	Name string

	From LinkFrom

	To commonv1.ObjectRef

	Capabilities []Capability
}

type LinkDeniedError struct {
	From commonv1.ObjectRef

	To commonv1.ObjectRef

	Capability Capability
}

func (e *LinkDeniedError) Error() string {
	if e == nil {
		return ErrLinkDenied.Error()
	}
	return fmt.Sprintf("bus: no accepted ComponentLink allows %s to call %s with %s", refString(e.From), refString(e.To), e.Capability)
}

func (e *LinkDeniedError) Unwrap() error {
	return ErrLinkDenied
}

func LinkAllows(link Link, from Caller, to commonv1.ObjectRef, c Capability) bool {
	if !link.From.matches(from) {
		return false
	}
	if !refMatches(link.To, to) {
		return false
	}
	for _, granted := range link.Capabilities {
		if strings.EqualFold(strings.TrimSpace(granted.Name), strings.TrimSpace(c.Name)) && scopeCovers(granted.Scope, c.Scope) {
			return true
		}
	}
	return false
}

func (f LinkFrom) matches(caller Caller) bool {
	if f.Ref != nil {
		return refMatches(*f.Ref, caller.Ref)
	}
	if f.Selector == nil || strings.TrimSpace(f.Kind) == "" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(f.Kind), strings.TrimSpace(caller.Ref.Kind)) {
		return false
	}
	if !groupMatches(f.Group, caller.Ref.Group) {
		return false
	}
	if strings.TrimSpace(f.Workspace) != strings.TrimSpace(caller.Ref.Workspace) {
		return false
	}
	return f.Selector.Matches(labels.Set(caller.Labels))
}

func refMatches(pattern, ref commonv1.ObjectRef) bool {
	if strings.TrimSpace(pattern.Kind) == "" || strings.TrimSpace(pattern.Name) == "" || strings.TrimSpace(pattern.Workspace) == "" {
		return false
	}
	return strings.TrimSpace(pattern.Workspace) == strings.TrimSpace(ref.Workspace) &&
		strings.EqualFold(strings.TrimSpace(pattern.Kind), strings.TrimSpace(ref.Kind)) &&
		strings.EqualFold(strings.TrimSpace(pattern.Name), strings.TrimSpace(ref.Name)) &&
		groupMatches(pattern.Group, ref.Group)
}

func groupMatches(pattern, group string) bool {
	pattern, group = strings.TrimSpace(pattern), strings.TrimSpace(group)
	return pattern == "" || group == "" || strings.EqualFold(pattern, group)
}

func scopeCovers(granted, requested []string) bool {
	if len(granted) == 0 || slices.Contains(granted, ScopeAll) {
		return true
	}
	if len(requested) == 0 {
		return false
	}
	for _, want := range requested {
		if !scopeEntryCovered(granted, strings.TrimSpace(want)) {
			return false
		}
	}
	return true
}

func scopeEntryCovered(granted []string, want string) bool {
	for _, entry := range granted {
		entry = strings.TrimSpace(entry)
		if entry == want {
			return true
		}
		if prefix, ok := strings.CutSuffix(entry, ScopeAll); ok && strings.HasPrefix(want, prefix) {
			return true
		}
	}
	return false
}

func refString(ref commonv1.ObjectRef) string {
	if ref.IsZero() {
		return "an unnamed caller"
	}
	return ref.String()
}

func cloneLinks(links []Link) []Link {
	if links == nil {
		return nil
	}
	out := make([]Link, len(links))
	for i, link := range links {
		out[i] = link
		if link.From.Ref != nil {
			ref := *link.From.Ref
			out[i].From.Ref = &ref
		}
		out[i].Capabilities = cloneCapabilities(link.Capabilities)
	}
	return out
}

func cloneCapabilities(in []Capability) []Capability {
	if in == nil {
		return nil
	}
	out := make([]Capability, len(in))
	for i, c := range in {
		out[i] = Capability{Name: c.Name, Scope: slices.Clone(c.Scope)}
	}
	return out
}

func linksEqual(a, b []Link) bool {
	return slices.EqualFunc(a, b, func(x, y Link) bool {
		if x.Workspace != y.Workspace || x.Name != y.Name || x.To != y.To {
			return false
		}
		if (x.From.Ref == nil) != (y.From.Ref == nil) || (x.From.Ref != nil && *x.From.Ref != *y.From.Ref) {
			return false
		}
		if x.From.Group != y.From.Group || x.From.Kind != y.From.Kind || x.From.Workspace != y.From.Workspace {
			return false
		}
		if selectorString(x.From.Selector) != selectorString(y.From.Selector) {
			return false
		}
		return slices.EqualFunc(x.Capabilities, y.Capabilities, func(p, q Capability) bool {
			return p.Name == q.Name && slices.Equal(p.Scope, q.Scope)
		})
	})
}

func selectorString(s labels.Selector) string {
	if s == nil {
		return ""
	}
	return s.String()
}

var deniedTotal = expvar.NewMap("socialweb_bus_link_denied_total")

type denials struct {
	count atomic.Uint64

	log atomic.Pointer[slog.Logger]
}

func (d *denials) deny(from Caller, to commonv1.ObjectRef, c Capability) error {
	d.count.Add(1)
	deniedTotal.Add(c.Name, 1)
	log := d.log.Load()
	if log == nil {
		log = slog.Default()
	}
	log.Warn("bus: link denied", "from", refString(from.Ref), "to", refString(to), "capability", c.String())
	return &LinkDeniedError{From: from.Ref, To: to, Capability: Capability{Name: c.Name, Scope: slices.Clone(c.Scope)}}
}
