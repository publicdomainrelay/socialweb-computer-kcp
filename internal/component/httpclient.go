package component

import (
	"net/http"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httpclient"
)

func (d Deps) ExternalHTTPClient(timeout time.Duration) *http.Client {
	return httpclient.External(httpclient.Options{
		Timeout: timeout,
		Hosts:   d.Hosts,
		TLS:     d.TLS,
		OnError: func(err error) {
			if d.Log != nil {
				d.Log.Warn("the external HTTP transport is not configured", "err", err)
			}
		},
	})
}
