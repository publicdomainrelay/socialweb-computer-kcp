package exposure

import (
	"sort"
	"sync"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
)

type LinkPolicy struct {
	mu sync.Mutex

	handlers *bus.Bus

	sources map[string]map[string][]bus.Link
}

func NewLinkPolicy(handlers *bus.Bus) *LinkPolicy {
	policy := &LinkPolicy{handlers: handlers, sources: map[string]map[string][]bus.Link{}}
	if handlers != nil {
		handlers.SetLinks(nil)
	}
	return policy
}

func (p *LinkPolicy) Set(workspace, name string, links []bus.Link) {
	if p == nil {
		return
	}
	p.mu.Lock()
	byName := p.sources[workspace]
	if byName == nil {
		byName = map[string][]bus.Link{}
		p.sources[workspace] = byName
	}
	byName[name] = append([]bus.Link(nil), links...)
	p.publishLocked()
	p.mu.Unlock()
}

func (p *LinkPolicy) Remove(workspace, name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	if byName := p.sources[workspace]; byName != nil {
		delete(byName, name)
		if len(byName) == 0 {
			delete(p.sources, workspace)
		}
	}
	p.publishLocked()
	p.mu.Unlock()
}

func (p *LinkPolicy) Links() []bus.Link {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mergedLocked()
}

func (p *LinkPolicy) publishLocked() {
	if p.handlers == nil {
		return
	}
	p.handlers.SetLinks(p.mergedLocked())
}

func (p *LinkPolicy) mergedLocked() []bus.Link {
	var out []bus.Link
	for _, byName := range p.sources {
		for _, links := range byName {
			out = append(out, links...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].Name < out[j].Name
	})
	return out
}
