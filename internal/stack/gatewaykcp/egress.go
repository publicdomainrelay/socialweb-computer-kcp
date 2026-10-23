package gatewaykcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	ListenerEgress = "egress"
	egressDialTime = 10 * time.Second
	egressRecord   = "egress.addr"
)

type egressMap struct {
	host   string
	target string
}

func guestTLSEgress(guestTLSURL string) (egressMap, bool, error) {
	if guestTLSURL == "" {
		return egressMap{}, false, nil
	}
	u, err := url.Parse(guestTLSURL)
	if err != nil {
		return egressMap{}, false, fmt.Errorf("gatewaykcp: the network value's guestTlsUrl %q: %w", guestTLSURL, err)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return egressMap{}, false, fmt.Errorf("gatewaykcp: the network value's guestTlsUrl %q names no host and port", guestTLSURL)
	}
	host, target := strings.ToLower(u.Hostname()), u.Host
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		target = net.JoinHostPort("127.0.0.1", u.Port())
	}
	return egressMap{host: host, target: target}, true, nil
}

func (m egressMap) dialAddress(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port != "443" {
		return hostport
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == m.host || strings.HasSuffix(h, "."+m.host) {
		return m.target
	}
	return hostport
}

func (m egressMap) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "only CONNECT", http.StatusMethodNotAllowed)
		return
	}
	upstream, err := net.DialTimeout("tcp", m.dialAddress(r.Host), egressDialTime)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			upstream.Write(b)
		}
		io.Copy(upstream, client)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(client, upstream)
		done <- struct{}{}
	}()
	<-done
	client.Close()
	upstream.Close()
	<-done
}

func listenEgress(record string) (net.Listener, error) {
	if b, err := os.ReadFile(record); err == nil {
		if ln, err := net.Listen("tcp", strings.TrimSpace(string(b))); err == nil {
			return ln, nil
		}
	}
	ln, err := net.Listen("tcp", DefaultAddress)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(record, []byte(ln.Addr().String()+"\n"), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func runEgress(ctx context.Context, m egressMap, record string, log *slog.Logger) (net.Addr, error) {
	ln, err := listenEgress(record)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: m, ReadHeaderTimeout: egressDialTime}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the egress proxy stopped", "err", err.Error())
		}
	}()
	return ln.Addr(), nil
}

const outboundCAFile = "outbound-ca-bundle.pem"

var systemCertFiles = []string{"/etc/ssl/certs/ca-certificates.crt", "/etc/pki/tls/certs/ca-bundle.crt", "/etc/ssl/ca-bundle.pem",
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", "/etc/ssl/cert.pem"}

func outboundCABundle(dir, networkCA string) (string, error) {
	ca := []byte(strings.TrimSpace(networkCA) + "\n")
	var system []byte
	for _, f := range systemCertFiles {
		if b, err := os.ReadFile(f); err == nil {
			system = b
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(dir, outboundCAFile)
	body := append(append(append([]byte{}, system...), '\n'), ca...)
	if err := os.WriteFile(out+".tmp", body, 0o600); err != nil {
		return "", err
	}
	return out, os.Rename(out+".tmp", out)
}
