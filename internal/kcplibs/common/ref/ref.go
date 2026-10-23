package ref

import "strings"

const APIPathPrefix = "/clusters/"

type Ref struct {
	LogicalCluster string

	Namespace string

	Name string

	ResourceVersion string
}

func New(logicalCluster, namespace, name string) Ref {
	return Ref{LogicalCluster: logicalCluster, Namespace: namespace, Name: name}
}

func Key(logicalCluster, namespace, name string) string {
	return logicalCluster + "/" + namespace + "/" + name
}

func (r Ref) Key() string {
	return Key(r.LogicalCluster, r.Namespace, r.Name)
}

func (r Ref) WithResourceVersion(version string) Ref {
	r.ResourceVersion = version
	return r
}

func (r Ref) WithNamespace(namespace string) Ref {
	r.Namespace = namespace
	return r
}

func BaseHost(host string) string {
	host = strings.TrimSuffix(host, "/")
	if before, _, ok := strings.Cut(host, APIPathPrefix); ok {
		return before
	}
	return host
}

func ClusterURL(host, logicalCluster string) string {
	return BaseHost(host) + APIPathPrefix + logicalCluster
}
