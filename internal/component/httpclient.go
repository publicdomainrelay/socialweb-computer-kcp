package component

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
)

type Egress struct {
	Hosts NetworkHosts
	TLS   *tls.Config
	Log   *slog.Logger
}

func (e Egress) Client(timeout time.Duration) *http.Client {
	return httpclient.External(httpclient.Options{
		Timeout: timeout,
		Hosts:   e.Hosts,
		TLS:     e.TLS,
		OnError: func(err error) {
			if e.Log != nil {
				e.Log.Warn("the external HTTP transport is not configured", "err", err)
			}
		},
	})
}
