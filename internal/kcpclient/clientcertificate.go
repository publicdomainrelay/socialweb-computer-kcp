package kcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	clusterClientCertificates = "clusterclientcertificates"

	certWatchIdle  = 10 * time.Minute
	certWatchReady = 3 * time.Second
	certWatchRetry = time.Second
)

func CertSerialKey(serial string) string {
	return strings.TrimLeft(strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(serial)), "0")
}

func ClusterClientCertificateName(serial string) string {
	return "ccc-" + CertSerialKey(serial)
}

type CertificateRecord struct {
	DID      string
	Cluster  string
	Serial   string
	NotAfter time.Time
	NoExpiry bool
}

type CertificateState struct {
	Present bool
	Revoked bool
}

func (c *Client) RecordCertificate(ctx context.Context, rec CertificateRecord) error {
	home := HomePath(rec.DID, rec.Cluster)
	cluster, err := c.readCluster(ctx, home, rec.Cluster)
	if err != nil {
		return fmt.Errorf("kcpclient: the cluster %s that owns the certificate record: %w", home, err)
	}
	r, err := c.resource(home, socialwebResource)
	if err != nil {
		return err
	}
	obj := v1alpha1.ClusterClientCertificate{
		TypeMeta: metav1.TypeMeta{APIVersion: socialwebResource.String(), Kind: "ClusterClientCertificate"},
		ObjectMeta: metav1.ObjectMeta{
			Name: ClusterClientCertificateName(rec.Serial),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: socialwebResource.String(), Kind: "Cluster",
				Name: cluster.Name, UID: cluster.UID}},
		},
		Spec: v1alpha1.ClusterClientCertificateSpec{ClusterRef: commonv1.LocalObjectRef{Name: rec.Cluster}, Did: rec.DID, Serial: CertSerialKey(rec.Serial),
			NotAfter: rec.NotAfter.UTC().Format(time.RFC3339), NoExpiry: rec.NoExpiry},
	}
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	err = r.Post().Resource(clusterClientCertificates).Body(body).Do(ctx).Error()
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("kcpclient: the certificate record in %s: %w", home, err)
	}
	return nil
}

func (c *Client) MarkCertificateRevoked(ctx context.Context, did, cluster, serial string) error {
	home := HomePath(did, cluster)
	r, err := c.resource(home, socialwebResource)
	if err != nil {
		return err
	}
	name := ClusterClientCertificateName(serial)
	spec, err := json.Marshal(map[string]any{"spec": map[string]any{"revoked": true}})
	if err != nil {
		return err
	}
	err = r.Patch(types.MergePatchType).Resource(clusterClientCertificates).Name(name).Body(spec).Do(ctx).Error()
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("kcpclient: revoke the certificate record %s in %s: %w", name, home, err)
	}
	status, err := json.Marshal(map[string]any{"status": map[string]any{"revokedAt": time.Now().UTC().Format(time.RFC3339)}})
	if err != nil {
		return err
	}
	err = r.Patch(types.MergePatchType).SubResource("status").Resource(clusterClientCertificates).Name(name).Body(status).Do(ctx).Error()
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("kcpclient: the revokedAt of the certificate record %s in %s: %w", name, home, err)
	}
	return nil
}

func (c *Client) CertificateState(ctx context.Context, did, cluster, serial string) (CertificateState, error) {
	w := c.certWatcher(did, cluster)
	w.touch()
	select {
	case <-w.ready:
	case <-time.After(certWatchReady):
		return CertificateState{}, fmt.Errorf("kcpclient: the certificate records of %s are not synced: %v", w.home, w.lastError())
	case <-ctx.Done():
		return CertificateState{}, ctx.Err()
	}
	return w.state(CertSerialKey(serial)), nil
}

type certWatch struct {
	home   string
	ready  chan struct{}
	once   sync.Once
	cancel context.CancelFunc

	mu    sync.Mutex
	items map[string]bool
	idle  *time.Timer
	err   error
}

func (w *certWatch) touch() {
	w.mu.Lock()
	idle := w.idle
	w.mu.Unlock()
	if idle != nil {
		idle.Reset(certWatchIdle)
	}
}

func (w *certWatch) lastError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *certWatch) state(key string) CertificateState {
	w.mu.Lock()
	defer w.mu.Unlock()
	revoked, ok := w.items[key]
	return CertificateState{Present: ok, Revoked: revoked}
}

func (w *certWatch) replace(items []v1alpha1.ClusterClientCertificate) {
	next := make(map[string]bool, len(items))
	for _, it := range items {
		next[CertSerialKey(it.Spec.Serial)] = it.Spec.Revoked
	}
	w.mu.Lock()
	w.items, w.err = next, nil
	w.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
}

func (w *certWatch) apply(deleted bool, it v1alpha1.ClusterClientCertificate) error {
	key := CertSerialKey(it.Spec.Serial)
	w.mu.Lock()
	defer w.mu.Unlock()
	if deleted {
		delete(w.items, key)
		return nil
	}
	w.items[key] = it.Spec.Revoked
	return nil
}

func (w *certWatch) fail(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

func (c *Client) certWatcher(did, cluster string) *certWatch {
	home := HomePath(did, cluster)
	c.certMu.Lock()
	defer c.certMu.Unlock()
	if w := c.certWatches[home]; w != nil {
		return w
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &certWatch{home: home, ready: make(chan struct{}), cancel: cancel, items: map[string]bool{}}
	if c.certWatches == nil {
		c.certWatches = map[string]*certWatch{}
	}
	c.certWatches[home] = w
	idle := time.AfterFunc(certWatchIdle, func() {
		c.certMu.Lock()
		if c.certWatches[home] == w {
			delete(c.certWatches, home)
		}
		c.certMu.Unlock()
		cancel()
	})
	w.mu.Lock()
	w.idle = idle
	w.mu.Unlock()
	go c.runCertWatch(ctx, w)
	return w
}

func (c *Client) runCertWatch(ctx context.Context, w *certWatch) {
	r, err := c.resource(w.home, socialwebResource)
	if err != nil {
		w.fail(err)
		return
	}
	src := source[v1alpha1.ClusterClientCertificate]{r: r, resource: clusterClientCertificates, where: w.home}
	for ctx.Err() == nil {
		items, rv, err := src.list(ctx)
		if err == nil {
			w.replace(items)
			err = follow(ctx, src, rv, w.apply, func(items []v1alpha1.ClusterClientCertificate) error {
				w.replace(items)
				return nil
			})
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			w.fail(err)
			slog.Warn("kcpclient: the certificate record watch restarts", "home", w.home, "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(certWatchRetry):
		}
	}
}
