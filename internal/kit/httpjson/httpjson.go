package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	DefaultLimit int64 = 8 << 20

	Unlimited int64 = -1

	statusDetailLimit = 300
)

var ErrTooLarge = errors.New("httpjson: the response body is too large")

type Response struct {
	Status int

	Header http.Header

	Body []byte
}

type StatusError struct {
	Method string

	URL string

	Status int

	Body []byte
}

func (e *StatusError) Error() string {
	text := strings.TrimSpace(string(e.Body))
	if len(text) > statusDetailLimit {
		text = text[:statusDetailLimit] + "..."
	}
	if text == "" {
		return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.URL, e.Status)
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, text)
}

type TooLargeError struct {
	Status int

	Limit int64
}

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("httpjson: the HTTP %d response is larger than %d bytes", e.Status, e.Limit)
}

func (e *TooLargeError) Is(target error) bool { return target == ErrTooLarge }

func NewRequest(ctx context.Context, method, target string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("httpjson: encoding %s %s: %w", method, target, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func Exchange(ctx context.Context, client *http.Client, req *http.Request, limit int64) (Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if ctx != nil && ctx != req.Context() {
		req = req.WithContext(ctx)
	}
	if limit == 0 {
		limit = DefaultLimit
	}
	resp, err := client.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if limit > 0 {
		reader = io.LimitReader(resp.Body, limit+1)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		return Response{}, err
	}
	if limit > 0 && int64(len(raw)) > limit {
		return Response{}, &TooLargeError{Status: resp.StatusCode, Limit: limit}
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: raw}, nil
}

func Do[Out any](ctx context.Context, client *http.Client, req *http.Request) (Out, error) {
	return DoLimit[Out](ctx, client, req, DefaultLimit)
}

func DoLimit[Out any](ctx context.Context, client *http.Client, req *http.Request, limit int64) (Out, error) {
	var out Out
	resp, err := Exchange(ctx, client, req, limit)
	if err != nil {
		return out, err
	}
	if resp.Status < 200 || resp.Status > 299 {
		return out, &StatusError{Method: req.Method, URL: req.URL.Redacted(), Status: resp.Status, Body: resp.Body}
	}
	switch target := any(&out).(type) {
	case *Response:
		*target = resp
		return out, nil
	case *[]byte:
		*target = resp.Body
		return out, nil
	}
	if len(bytes.TrimSpace(resp.Body)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return out, fmt.Errorf("httpjson: decoding %s %s: %w", req.Method, req.URL.Redacted(), err)
	}
	return out, nil
}
