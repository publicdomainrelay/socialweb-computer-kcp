package kcp

import (
	"cmp"
	"strings"
)

const (
	RootWorkspace = "root"

	ClusterAnnotation = "kcp.io/cluster"

	PathAnnotation = "kcp.io/path"

	DefaultServiceDomain = "kcp.local"

	ServiceSegment = "svc"
)

func ServiceLabels(logicalCluster string) string {
	parts := strings.Split(logicalCluster, ":")
	out := make([]string, 0, len(parts))
	for i := len(parts) - 1; i >= 0; i-- {
		if parts[i] == "" || parts[i] == RootWorkspace {
			continue
		}
		out = append(out, parts[i])
	}
	return strings.Join(out, ".")
}

func ServiceFQDN(name, namespace, logicalCluster, domain string) string {
	labels := ServiceLabels(logicalCluster)
	namespace = cmp.Or(namespace, "default")
	host := name + "." + namespace
	if labels != "" {
		host += "." + labels
	}
	return host + "." + ServiceSegment + "." + domain
}

func KindServiceHost(kind, logicalCluster string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == "" {
		return ""
	}
	return ServiceFQDN(kind, "default", logicalCluster, DefaultServiceDomain)
}

func HandlerURL(logicalCluster, kind, name string) string {
	host := KindServiceHost(kind, logicalCluster)
	if host == "" || strings.TrimSpace(name) == "" {
		return ""
	}
	return "http://" + host
}
