package pdsclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/kcp"
)

type LocalRef struct {
	Workspace string
	Kind      string
	Name      string
	Endpoint  string
}

type LocalConfig struct {
	Handlers *component.HandlerRegistry
	BaseURL  string
	Tokens   TokenSource
	DPoP     DPoPSource
	Timeout  time.Duration
}

type LocalPDS struct {
	*basePDS
}

var _ PDS = (*LocalPDS)(nil)
var _ Ref = LocalRef{}

func NewLocal(cfg LocalConfig) (*LocalPDS, error) {
	if cfg.Handlers == nil {
		return nil, fmt.Errorf("%w: local handler registry is required", ErrInvalidConfig)
	}
	baseURL, err := parseBaseURL(cfg.BaseURL, "http")
	if err != nil {
		return nil, err
	}
	httpClient := cloneNoRedirect(cfg.Handlers.HTTPClient())
	client := newXRPCClient(baseURL, httpClient, cfg.Tokens, cfg.DPoP, cfg.Timeout)
	handlers := cfg.Handlers
	dial := func(ctx context.Context, cursor *int64) (RepoEventStream, error) {
		return newLocalSubscribe(ctx, handlers, baseURL, cursor)
	}
	return &LocalPDS{basePDS: &basePDS{client: client, dial: dial}}, nil
}

func (r LocalRef) baseURL() (string, error) {
	endpoint := strings.TrimSpace(r.Endpoint)
	if endpoint != "" {
		if !strings.Contains(endpoint, "://") {
			endpoint = "http://" + endpoint
		}
		parsed, err := parseBaseURL(endpoint, "http")
		if err != nil {
			return "", err
		}
		return parsed.String(), nil
	}
	kind := strings.ToLower(strings.TrimSpace(r.Kind))
	if kind == "" {
		return "", fmt.Errorf("%w: local kind is required", ErrInvalidConfig)
	}
	name := strings.ToLower(strings.TrimSpace(r.Name))
	if name != "" {
		return "http://" + kind + "." + name, nil
	}
	workspace := strings.TrimSpace(r.Workspace)
	if workspace != "" {
		return "http://" + kcp.ServiceFQDN(kind, "default", workspace, kcp.DefaultServiceDomain), nil
	}
	return "", fmt.Errorf("%w: local name or workspace is required", ErrInvalidConfig)
}
