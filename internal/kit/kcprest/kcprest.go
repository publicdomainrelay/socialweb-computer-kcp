package kcprest

import (
	"strings"

	"k8s.io/client-go/rest"
)

func ForCluster(base *rest.Config, ws string) *rest.Config {
	cfg := rest.CopyConfig(base)
	host, _, _ := strings.Cut(strings.TrimSuffix(cfg.Host, "/"), "/clusters/")
	cfg.Host = host + "/clusters/" + ws
	return cfg
}
