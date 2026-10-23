package participant

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/xrpc"
)

const OutboundReplyLimit = 4096

type CallError struct {
	Status int

	Err error
}

func (e *CallError) HTTPStatus() int { return e.Status }

func (e *CallError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

func (e *CallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func Rejected(err error) bool {
	var callErr *CallError
	return errors.As(err, &callErr) && callErr.Status >= 400 && callErr.Status <= 499 && callErr.Status != http.StatusRequestTimeout &&
		callErr.Status != http.StatusTooManyRequests
}

type Outbound struct {
	HTTP *http.Client

	Tokens *Tokens

	Log *slog.Logger
}

func (o *Outbound) log() *slog.Logger {
	if o == nil || o.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return o.Log
}

func (o *Outbound) Post(ctx context.Context, endpoint, nsid string, body any) error {
	host := HostPort(endpoint)
	client := &xrpc.Client{Host: endpoint, HTTP: o.HTTP, Limit: OutboundReplyLimit}
	if host == "" {
		o.log().Warn("the outbound XRPC call is unsigned: the endpoint has no hostname", "url", endpoint, "nsid", nsid)
	} else if _, err := o.Tokens.Token(ctx, host, nsid); err != nil {
		o.log().Warn("the outbound XRPC call is unsigned: the PDS cannot mint a service-auth token", "url", endpoint, "nsid", nsid, "err", err)
	} else {
		client.Auth = &xrpc.ServiceAuth{Tokens: o.Tokens.Source(host, nsid)}
	}
	_, err := xrpc.Procedure[any, struct{}](ctx, client, nsid, body)
	if xerr, ok := errors.AsType[*xrpc.Error](err); ok {
		return &CallError{Status: xerr.Status, Err: xerr}
	}
	return err
}

func HostPort(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
