package componentcore

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

const scopeCacheTTL = 10 * time.Second

type workspaceScope struct {
	static []string

	dynamic func(context.Context) []string

	mu sync.Mutex

	cached []string

	at time.Time
}

func newWorkspaceScope(static []string, dynamic func(context.Context) []string) *workspaceScope {
	out := make([]string, 0, len(static))
	for _, workspace := range static {
		if workspace = strings.TrimSpace(workspace); workspace != "" && !slices.Contains(out, workspace) {
			out = append(out, workspace)
		}
	}
	return &workspaceScope{static: out, dynamic: dynamic}
}

func (s *workspaceScope) base() string {
	if len(s.static) == 0 {
		return DefaultWorkspace
	}
	return s.static[0]
}

func (s *workspaceScope) Base(workspace string) bool {
	return strings.TrimSpace(workspace) == s.base()
}

func (s *workspaceScope) Contains(workspace string) bool {
	workspace = strings.TrimSpace(workspace)
	if slices.Contains(s.static, workspace) {
		return true
	}
	if s.dynamic == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil || time.Since(s.at) > scopeCacheTTL {
		ctx, cancel := context.WithTimeout(context.Background(), scopeCacheTTL)
		s.cached = append([]string{}, s.dynamic(ctx)...)
		cancel()
		s.at = time.Now()
	}
	return slices.Contains(s.cached, workspace)
}
