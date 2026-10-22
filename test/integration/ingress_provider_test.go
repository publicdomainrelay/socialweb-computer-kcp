//go:build integration

package integration

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/services/ingressproxy/opentunnel"
)

func TestOpenTunnelProviderPublication(t *testing.T) {
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "url")
	script := filepath.Join(dir, "fake-opentunnel.sh")
	body := "#!/bin/sh\ncat " + urlFile + "\nexec sleep 300\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urlFile, []byte("https://pds-one.opentunnel.xyz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	handlers := component.NewHandlerRegistry()
	target := component.Key{Workspace: "root", Kind: "Pds", Name: "pds"}
	if err := handlers.RegisterHTTP(target, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})); err != nil {
		t.Fatal(err)
	}
	provider := opentunnel.New(opentunnel.Config{Command: []string{script}, RouteName: "pds-test", LocalHost: "127.0.0.1"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := provider.Start(ctx, component.Deps{Handlers: handlers, Log: slog.New(slog.DiscardHandler), StateDir: dir}); err != nil {
			t.Errorf("provider start: %v", err)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := provider.Ready(ctx); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	spec := exposure.IngressSpec{Workspace: "root", Exposure: "pds-ingress", Service: "pds", Protocol: exposurev1alpha1.ServiceProtocolHTTP,
		Handler: "http", Target: target, IngressRef: "ingress"}
	registration, err := provider.Register(ctx, spec)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := registration.URL(); !strings.HasPrefix(got, "https://pds-one.opentunnel.xyz") {
		t.Fatalf("URL = %q", got)
	}
	if err := os.WriteFile(urlFile, []byte("https://pds-two.opentunnel.xyz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := provider.Register(ctx, spec)
	if err == nil {
		_ = second.Close()
		t.Fatal("a changed public URL was accepted")
	}
	if !errors.Is(err, exposure.ErrStableHostMismatch) {
		t.Fatalf("second register error = %v, want StableHostMismatch", err)
	}
	_ = registration.Close()
	cancel()
	<-done
}
