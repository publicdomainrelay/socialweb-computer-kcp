package httptunnel

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
)

const ResponseLimit = 32 << 20

type Protocol struct {
	Dialer tunnel.Dialer
}

func (p *Protocol) RoundTrip(req *http.Request) (*http.Response, error) {
	if p == nil || p.Dialer == nil {
		return nil, errors.New("httptunnel: no dialer")
	}
	if req.URL == nil || req.URL.Scheme != tunnel.KindWebRTC {
		return nil, errors.New("httptunnel: the request is not a webrtc endpoint")
	}
	ctx := req.Context()
	conn, err := p.Dialer.DialContext(ctx, tunnel.NetworkWebRTC, req.URL.Host)
	if err != nil {
		return nil, err
	}
	cancel := context.AfterFunc(ctx, func() {
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	out := req.Clone(ctx)
	out.URL.Scheme = "http"
	out.URL.Host = req.URL.Host
	out.Host = req.URL.Host
	out.Close = true
	if err := out.Write(conn); err != nil {
		cancel()
		_ = conn.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), out)
	if err != nil {
		cancel()
		_ = conn.Close()
		return nil, err
	}
	response.Body = &body{ReadCloser: response.Body, conn: conn, cancel: cancel}
	return response, nil
}

type body struct {
	io.ReadCloser

	conn net.Conn

	cancel func() bool

	once sync.Once
}

func (b *body) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(func() {
		b.cancel()
		_ = b.conn.Close()
	})
	return err
}

func Serve(ctx context.Context, conn net.Conn, handler http.Handler) error {
	if conn == nil {
		return errors.New("httptunnel: no connection")
	}
	if handler == nil {
		return errors.New("httptunnel: no handler")
	}
	request, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return err
	}
	request = request.WithContext(ctx)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, ResponseLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > ResponseLimit {
		return errors.New("httptunnel: the response is too large")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
	response.Header.Del("Transfer-Encoding")
	response.Header.Set("Content-Length", strconv.Itoa(len(raw)))
	response.Close = true
	return response.Write(conn)
}
