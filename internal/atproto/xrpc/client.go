package xrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const (
	DefaultLimit int64 = 1 << 20

	Prefix = "/xrpc/"

	HealthPath = Prefix + "_health"
)

var ErrTooLarge = errors.New("xrpc: the response body is too large")

type Client struct {
	Host string

	HTTP *http.Client

	Auth Auth

	Header http.Header

	Limit int64
}

type Reply struct {
	Status int

	Header http.Header

	Body []byte
}

func Query[Out any](ctx context.Context, c *Client, nsid string, params url.Values) (Out, error) {
	var out Out
	target, err := c.target(nsid, params)
	if err != nil {
		return out, err
	}
	reply, err := c.do(ctx, http.MethodGet, nsid, target, nil)
	return decode[Out](nsid, reply, err)
}

func Procedure[In, Out any](ctx context.Context, c *Client, nsid string, in In) (Out, error) {
	var out Out
	target, err := c.target(nsid, nil)
	if err != nil {
		return out, err
	}
	body, err := json.Marshal(in)
	if err != nil {
		return out, fmt.Errorf("xrpc: encoding %s: %w", nsid, err)
	}
	reply, err := c.do(ctx, http.MethodPost, nsid, target, body)
	return decode[Out](nsid, reply, err)
}

func Health(ctx context.Context, c *Client) error {
	if c == nil || strings.TrimSpace(c.Host) == "" {
		return errors.New("xrpc: the client has no host")
	}
	reply, err := c.do(ctx, http.MethodGet, "_health", c.base()+HealthPath, nil)
	if err != nil {
		return err
	}
	if reply.Status != http.StatusOK {
		return &Error{Status: reply.Status, Body: reply.Body}
	}
	return nil
}

func (c *Client) base() string {
	return strings.TrimSuffix(strings.TrimRight(c.Host, "/"), "/xrpc")
}

func (c *Client) target(nsid string, params url.Values) (string, error) {
	if c == nil || strings.TrimSpace(c.Host) == "" {
		return "", errors.New("xrpc: the client has no host")
	}
	if _, err := syntax.ParseNSID(nsid); err != nil {
		return "", fmt.Errorf("xrpc: %w", err)
	}
	target := c.base() + Prefix + nsid
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	return target, nil
}

func (c *Client) do(ctx context.Context, method, nsid, target string, body []byte) (Reply, error) {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	limit := c.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	nonceRetried := false
	authRetried := false
	var rejected error
	for {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return Reply{}, fmt.Errorf("xrpc: building %s: %w", nsid, err)
		}
		for key, values := range c.Header {
			req.Header[key] = append([]string(nil), values...)
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.Auth != nil {
			if err := c.Auth.Apply(req); err != nil {
				if rejected != nil {
					return Reply{}, fmt.Errorf("%w (renewing the credentials: %v)", rejected, err)
				}
				return Reply{}, err
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			return Reply{}, fmt.Errorf("xrpc: %s: %w", nsid, err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		resp.Body.Close()
		if readErr != nil {
			return Reply{}, fmt.Errorf("xrpc: reading %s: %w", nsid, readErr)
		}
		over := int64(len(raw)) > limit
		if over {
			raw = raw[:limit]
		}
		if c.Auth != nil {
			switch c.verdict(resp) {
			case RetryNonce:
				if !nonceRetried {
					nonceRetried = true
					continue
				}
			case RetryAuth:
				if !authRetried {
					authRetried = true
					rejected = newError(resp.StatusCode, raw)
					c.Auth.Invalidate()
					continue
				}
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return Reply{}, newError(resp.StatusCode, raw)
		}
		reply := Reply{Status: resp.StatusCode, Header: resp.Header, Body: raw}
		if over {
			return reply, fmt.Errorf("%w: %s answered more than %d bytes", ErrTooLarge, nsid, limit)
		}
		return reply, nil
	}
}

func (c *Client) verdict(resp *http.Response) Verdict {
	if challenger, ok := c.Auth.(Challenger); ok {
		return challenger.Challenge(resp)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return RetryAuth
	}
	return Done
}

func decode[Out any](nsid string, reply Reply, err error) (Out, error) {
	var out Out
	if _, discard := any(&out).(*struct{}); discard && errors.Is(err, ErrTooLarge) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	switch target := any(&out).(type) {
	case *Reply:
		*target = reply
		return out, nil
	case *struct{}:
		return out, nil
	}
	if len(bytes.TrimSpace(reply.Body)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(reply.Body, &out); err != nil {
		return out, fmt.Errorf("xrpc: decoding %s: %w", nsid, err)
	}
	return out, nil
}
