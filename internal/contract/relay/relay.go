package relay

import (
	"context"
	"net/http"
)

type Crawler interface {
	Crawl(hostname string)

	Forget(hostname string)
}

type Relay interface {
	Crawler

	Handler() http.Handler

	Run(ctx context.Context) error
}
