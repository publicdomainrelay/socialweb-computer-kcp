package tlsfront

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	CertName       = "tls.crt"
	KeyName        = "tls.key"
	SelfSignedCN   = "socialweb.local"
	SelfSignedLife = 10 * 365 * 24 * time.Hour
	DialTimeout    = 10 * time.Second
	HandshakeLimit = 15 * time.Second
)

var SelfSignedNames = []string{"localhost", "socialweb.local"}

type Config struct {
	Address  string
	CertFile string
	KeyFile  string
	Backend  func() string
	OnListen func(net.Addr)
	Log      *slog.Logger
}

func (c Config) log() *slog.Logger {
	if c.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return c.Log.With("component", "tls-front")
}

func EnsureSelfSigned(dir string) (string, string, error) {
	return EnsurePair(dir, CertName, KeyName, func() ([]byte, []byte, error) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, nil, err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
		if err != nil {
			return nil, nil, err
		}
		now := time.Now()
		tmpl := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: SelfSignedCN},
			NotBefore: now.Add(-time.Minute), NotAfter: now.Add(SelfSignedLife), DNSNames: SelfSignedNames,
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
		if err != nil {
			return nil, nil, err
		}
		kder, err := x509.MarshalECPrivateKey(priv)
		if err != nil {
			return nil, nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
	})
}

func EnsurePair(dir, certName, keyName string, generate func() ([]byte, []byte, error)) (string, string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	unlock, err := lockDir(dir)
	if err != nil {
		return "", "", err
	}
	defer unlock()
	cert, key := filepath.Join(dir, certName), filepath.Join(dir, keyName)
	if pairOK(cert, key) {
		return cert, key, nil
	}
	certPEM, keyPEM, err := generate()
	if err != nil {
		return "", "", err
	}
	if err := writeFile(key, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	if err := writeFile(cert, certPEM, 0o644); err != nil {
		return "", "", err
	}
	return cert, key, nil
}

func pairOK(cert, key string) bool {
	if st, err := os.Stat(cert); err != nil || st.Size() == 0 {
		return false
	}
	if st, err := os.Stat(key); err != nil || st.Size() == 0 {
		return false
	}
	_, err := tls.LoadX509KeyPair(cert, key)
	return err == nil
}

func lockDir(dir string) (func(), error) {
	file, err := os.OpenFile(filepath.Join(dir, ".tls.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func Fingerprint(certFile string) (string, error) {
	raw, err := os.ReadFile(certFile)
	if err != nil {
		return "", err
	}
	b, _ := pem.Decode(raw)
	if b == nil {
		return "", fmt.Errorf("tlsfront: %s holds no PEM certificate", certFile)
	}
	sum := sha256.Sum256(b.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

func Run(ctx context.Context, c Config) error {
	if c.Backend == nil {
		return errors.New("tlsfront: no backend")
	}
	var tlsConfig *tls.Config
	if c.CertFile != "" || c.KeyFile != "" {
		pair, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		if err != nil {
			return fmt.Errorf("tlsfront: the certificate %s and key %s: %w", c.CertFile, c.KeyFile, err)
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	ln, err := net.Listen("tcp", c.Address)
	if err != nil {
		return fmt.Errorf("tlsfront: listening on %s: %w", c.Address, err)
	}
	if tlsConfig != nil {
		fp, _ := Fingerprint(c.CertFile)
		c.log().Info("listening", "addr", ln.Addr().String(), "cert", c.CertFile, "sha256", fp)
		ln = tls.NewListener(ln, tlsConfig)
	} else {
		c.log().Info("listening without TLS", "addr", ln.Addr().String())
	}
	if c.OnListen != nil {
		c.OnListen(ln.Addr())
	}
	var wg sync.WaitGroup
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("tlsfront: accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			proxy(ctx, c, conn)
		}()
	}
}

func proxy(ctx context.Context, c Config, conn net.Conn) {
	defer conn.Close()
	if tc, ok := conn.(*tls.Conn); ok {
		hctx, cancel := context.WithTimeout(ctx, HandshakeLimit)
		err := tc.HandshakeContext(hctx)
		cancel()
		if err != nil {
			c.log().Debug("handshake", "remote", conn.RemoteAddr().String(), "err", err.Error())
			return
		}
	}
	backend := c.Backend()
	if backend == "" {
		c.log().Warn("no backend address yet; the connection is closed", "remote", conn.RemoteAddr().String())
		return
	}
	d := net.Dialer{Timeout: DialTimeout}
	up, err := d.DialContext(ctx, "tcp", backend)
	if err != nil {
		c.log().Warn("dialing the backend", "backend", backend, "err", err.Error())
		return
	}
	defer up.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close(); up.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(up, conn)
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, up)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}
