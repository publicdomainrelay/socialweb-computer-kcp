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
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/devinstance"
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

func runEgress(ctx context.Context, m egressMap, record string, log *slog.Logger) (net.Addr, func(), error) {
	ln, err := listenEgress(record)
	if err != nil {
		return nil, nil, err
	}
	srv := &http.Server{Handler: m, ReadHeaderTimeout: egressDialTime}
	closeOnDone := context.AfterFunc(ctx, func() { srv.Close() })
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("the egress proxy stopped", "err", err.Error())
		}
	})
	stop := func() {
		closeOnDone()
		srv.Close()
		wg.Wait()
	}
	return ln.Addr(), stop, nil
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

func NetworkOutbound(ctx context.Context, n *v1alpha1.MarketNetworkValue, tlsDir, record string, log *slog.Logger) (net.Addr, func(), error) {
	if n != nil && n.CABundle != "" && os.Getenv("SSL_CERT_FILE") == "" {
		bundle, err := outboundCABundle(tlsDir, n.CABundle)
		if err != nil {
			return nil, func() {}, err
		}
		os.Setenv("SSL_CERT_FILE", bundle)
		log.Info("the system roots and the market network's CA are this gateway's outbound trust: every vault it places verifies a bidder's issuer with them",
			"bundle", bundle)
	}
	if n == nil {
		return nil, func() {}, nil
	}
	m, ok, err := guestTLSEgress(n.GuestTLSURL())
	if err != nil || !ok {
		return nil, func() {}, err
	}
	addr, stop, err := runEgress(ctx, m, record, log)
	if err != nil {
		return nil, func() {}, err
	}
	os.Setenv("HTTPS_PROXY", "http://"+addr.String())
	log.Info("the market network's guest TLS front is this gateway's egress for its port-less names: every vault it places fetches a bidder's issuer through it",
		"egress", addr.String(), "names", "*."+m.host+":443", "target", m.target)
	return addr, stop, nil
}

func RequesterEgressRecord(l devinstance.Layout) string {
	return filepath.Join(l.KcpDir(), egressRecord)
}
