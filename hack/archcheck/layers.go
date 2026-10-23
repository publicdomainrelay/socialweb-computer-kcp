package main

import (
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

var layerRank = map[string]int{
	"api":         0,
	"assets":      0,
	"contract":    1,
	"kit":         2,
	"transport":   3,
	"impl":        4,
	"controllers": 5,
	"app":         6,
	"cmd":         7,
}

const layerOrder = "api <- contract <- kit <- transport <- impl <- controllers <- app <- cmd"

var appRoots = []string{"internal/app", "internal/stack", "internal/devcore"}

var toolRoots = []string{"cmd", "test", "hack", "internal/apigen", "internal/lexgen"}

var backendPrefixes = []string{
	"internal/compute", "internal/vault", "internal/pds", "internal/atprotorepo", "internal/events",
	"internal/vmruntime", "internal/hostsvc", "internal/fcapi", "internal/guestnet", "internal/guestdisk", "internal/guestimage",
	"internal/policyengine/runs", "internal/relay", "internal/ingress", "internal/ssh", "internal/dispatcher",
	"internal/gateway", "internal/guestcp", "internal/market", "internal/lease", "internal/postgresctl", "internal/ttlctl",
	"internal/clusterprovider",
}

var hostLibraries = []string{
	"internal/vmruntime", "internal/hostsvc", "internal/fcapi", "internal/guestnet", "internal/guestdisk", "internal/guestimage",
	"internal/postgresctl", "internal/ttlctl", "internal/clusterprovider",
}

var backendBanned = []string{
	"k8s.io/client-go", "sigs.k8s.io/controller-runtime", "sigs.k8s.io/multicluster-runtime", "github.com/kcp-dev",
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured",
	"internal/bus", "internal/controllers", "internal/app", "internal/kcpserver", "internal/kcpclient",
	"internal/kcpdatastore", "internal/stack",
}

func (c *checker) layerOf(path string) string {
	if !c.local(path) {
		return ""
	}
	rel := c.rel(path)
	switch {
	case under(rel, "api"):
		return "api"
	case under(rel, "internal/contract"):
		return "contract"
	case under(rel, "internal/kit"):
		return "kit"
	case strings.HasPrefix(rel, "internal/controllers/runtime/"):
		return "transport"
	case under(rel, "internal/controllers"):
		return "controllers"
	case under(rel, appRoots...) || mainPackage(rel):
		return "app"
	case under(rel, toolRoots...) || rel == ".":
		return "cmd"
	case under(rel, "internal"):
		return "impl"
	}
	return "assets"
}

func mainPackage(rel string) bool {
	name, ok := strings.CutPrefix(rel, "internal/")
	return ok && !strings.Contains(name, "/") && strings.HasSuffix(name, "main")
}

func (c *checker) layers() {
	for _, p := range c.sortedLocal() {
		from := c.layerOf(p.PkgPath)
		for _, imp := range sortedImports(p) {
			if !c.local(imp) {
				if from == "contract" && strings.Contains(strings.SplitN(imp, "/", 2)[0], ".") {
					c.fail("layers", c.rel(p.PkgPath), "imports %s; a contract imports api and the standard library only", imp)
				}
				continue
			}
			to := c.layerOf(imp)
			switch {
			case from == "contract" && to != "api" && to != "contract":
				c.fail("layers", c.rel(p.PkgPath), "imports %s (%s); a contract imports api and the standard library only", c.rel(imp), to)
			case layerRank[to] > layerRank[from]:
				c.fail("layers", c.rel(p.PkgPath), "(%s) imports %s (%s); imports point down %s", from, c.rel(imp), to, layerOrder)
			}
		}
	}
}

func (c *checker) isBackend(path string) bool {
	return c.local(path) && under(c.rel(path), backendPrefixes...)
}

func (c *checker) banned(path string) string {
	name := path
	if c.local(path) {
		name = c.rel(path)
	}
	for _, b := range backendBanned {
		if under(name, b) {
			return b
		}
	}
	return ""
}

func (c *checker) backends() {
	for _, p := range c.sortedLocal() {
		if !c.isBackend(p.PkgPath) {
			continue
		}
		parent := map[string]string{p.PkgPath: ""}
		queue := []string{p.PkgPath}
		reported := map[string]bool{}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur != p.PkgPath {
				if b := c.banned(cur); b != "" {
					if !reported[b] {
						reported[b] = true
						c.fail("backend", c.rel(p.PkgPath), "depends on %s via %s; a backend package runs without kcp, the bus or a controller runtime", b, c.chain(parent, cur))
					}
					continue
				}
			}
			dep := c.byPath[cur]
			if dep == nil {
				continue
			}
			for _, imp := range sortedImports(dep) {
				if _, seen := parent[imp]; !seen {
					parent[imp] = cur
					queue = append(queue, imp)
				}
			}
		}
	}
}

func (c *checker) chain(parent map[string]string, at string) string {
	var hops []string
	for cur := at; cur != ""; cur = parent[cur] {
		name := cur
		if c.local(cur) {
			name = c.rel(cur)
		}
		hops = append(hops, name)
	}
	for i, j := 0, len(hops)-1; i < j; i, j = i+1, j-1 {
		hops[i], hops[j] = hops[j], hops[i]
	}
	return strings.Join(hops, " -> ")
}

func (c *checker) sortedLocal() []*packages.Package {
	var out []*packages.Package
	for path, p := range c.byPath {
		if c.local(path) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PkgPath < out[j].PkgPath })
	return out
}

func sortedImports(p *packages.Package) []string {
	out := make([]string, 0, len(p.Imports))
	for imp := range p.Imports {
		out = append(out, imp)
	}
	sort.Strings(out)
	return out
}
