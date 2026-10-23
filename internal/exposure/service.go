package exposure

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
)

var ErrUnknownProtocol = errors.New("unknown protocol")

func Target(ref commonv1.ObjectRef, workspace string) (component.Key, error) {
	kind := strings.TrimSpace(ref.Kind)
	name := strings.TrimSpace(ref.Name)
	if kind == "" {
		return component.Key{}, errors.New("the targetRef has no kind")
	}
	if name == "" {
		return component.Key{}, errors.New("the targetRef has no name")
	}
	targetWorkspace := cmp.Or(strings.TrimSpace(ref.Workspace), workspace)
	return component.Key{Workspace: targetWorkspace, Kind: kind, Name: name}, nil
}

func protocolKind(value exposurev1alpha1.ServiceProtocol) string {
	return strings.ToLower(strings.TrimSpace(string(value)))
}

func HandlerName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return component.DefaultHandlerName
	}
	return value
}

func FindHandler(ctx context.Context, handlers *component.Handlers, key component.Key, protocol exposurev1alpha1.ServiceProtocol, name string) (http.Handler, bool, error) {
	if handlers == nil {
		return nil, false, nil
	}
	switch protocolKind(protocol) {
	case component.ProtocolHTTP, component.ProtocolWS:
		handler, ok := handlers.Handler(key, name)
		return handler, ok, nil
	case component.ProtocolTCP, component.ProtocolSSH:
		conn, err := handlers.DialStream(ctx, key, name)
		if err != nil {
			if errors.Is(err, component.ErrNoStream) || errors.Is(err, component.ErrHandlersStopped) {
				return nil, false, nil
			}
			return nil, false, err
		}
		_ = conn.Close()
		return nil, true, nil
	default:
		return nil, false, fmt.Errorf("%w %q", ErrUnknownProtocol, protocol)
	}
}

func Endpoints(registry *component.Registry, key component.Key) []string {
	if registry == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, endpoint := range registry.Endpoints(key.Workspace, key.Kind, key.Name) {
		value := strings.TrimSpace(endpoint.URL)
		if value == "" && endpoint.Address != "" {
			protocol := strings.TrimSpace(endpoint.Protocol)
			if protocol != "" {
				value = protocol + "://" + endpoint.Address
			} else {
				value = endpoint.Address
			}
		}
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
