package dispatcher

import "net/http"

type Proxy interface {
	http.Handler

	Hosts() []string
}
