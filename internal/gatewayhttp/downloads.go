package gatewayhttp

import (
	"log/slog"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/pinneddownloads"
)

const DownloadsRoute = "GET /downloads/{sha256}/{name}"

type DownloadsOptions struct {
	Downloads *pinneddownloads.Set

	Log *slog.Logger
}

type Downloads struct {
	handler *pinneddownloads.Handler
}

func NewDownloads(opts DownloadsOptions) *Downloads {
	return &Downloads{handler: pinneddownloads.NewHandler(opts.Downloads, opts.Log)}
}

func (d *Downloads) Register(mux *http.ServeMux) {
	d.handler.Register(mux, DownloadsRoute)
}

func (d *Downloads) Handler() http.Handler {
	mux := http.NewServeMux()
	d.Register(mux)
	return mux
}
