package clusterprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/hostprocess"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/openbaoctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/pgunit"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/postgresctl"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const (
	LabelCluster       = "socialweb.computer/cluster"
	LabelMember        = "socialweb.computer/member"
	AnnotationVerified = "socialweb.computer/verified-invocation"
	MemberFinalizer    = "socialweb.computer/cluster-member"
	ControlPlaneFile   = "controlplane.json"
	ActiveSlot         = "a"

	DefaultPlaceTimeout = time.Minute
	DefaultStoreTimeout = time.Minute
	DefaultReapTimeout  = time.Minute
	MemberOutputCap     = 64 << 20
	memberStopGrace     = 30
	FrontFDStore        = 32
	frontStopGrace      = 10
	memberOutputTail    = 1 << 20
	unitPoll            = 500 * time.Millisecond
	placePoll           = 100 * time.Millisecond
)

var ErrMemberLost = errors.New("clusterprovider: a member unit was lost while it was being placed")

type UnitsConfig struct {
	RestConfig *rest.Config

	Host string

	Workspace string

	Prefix string

	StateDir string

	VaultStateDir string

	Resolve func(name string) (string, error)

	PlaceTimeout time.Duration

	StoreTimeout time.Duration

	ReapTimeout time.Duration

	Log *slog.Logger
}

type UnitsHost struct {
	c   UnitsConfig
	hps dynamic.ResourceInterface
	pgs dynamic.ResourceInterface
	obs dynamic.ResourceInterface

	mu     sync.Mutex
	stores map[string]Store
	keys   map[string]*sync.Mutex
}

type Store struct {
	Object    string
	Unit      string
	SocketDir string
	User      string
}

func (s Store) DSN() string { return pgunit.DSN(s.SocketDir, s.User, PostgresDatabase) }

func (s Store) VaultDSN() string { return pgunit.DSN(s.SocketDir, s.User, PostgresVaultDatabase) }

func (s Store) HasBootstrapRow(ctx context.Context) (bool, error) {
	rows, err := pgunit.Query(ctx, s.SocketDir, s.User, PostgresDatabase,
		"select count(*) from kine where name like '/bootstrap/%'")
	if err != nil {
		return false, fmt.Errorf("clusterprovider: reading k3s's bootstrap row out of %s: %w", s.Object, err)
	}
	if len(rows) == 0 || len(rows[0]) == 0 {
		return false, fmt.Errorf("clusterprovider: %s answered no row count", s.Object)
	}
	n, err := strconv.Atoi(rows[0][0])
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s Store) DeleteBootstrapRows(ctx context.Context) error {
	_, err := pgunit.Query(ctx, s.SocketDir, s.User, PostgresDatabase, "delete from kine where name like '/bootstrap/%'")
	return err
}

type ControlPlane struct {
	ActiveSlot string                `json:"activeSlot"`
	Slice      string                `json:"slice"`
	Store      string                `json:"store"`
	Members    []ControlPlaneMember  `json:"members"`
	Front      *ControlPlaneFront    `json:"front,omitempty"`
	Slots      map[string]SlotRecord `json:"slots,omitempty"`
	Links      map[string]LinkRecord `json:"links,omitempty"`
	LastRoll   string                `json:"lastRoll,omitempty"`
	WrittenAt  string                `json:"writtenAt"`
}

type SlotRecord struct {
	K3s        string `json:"k3s"`
	Kine       string `json:"kine"`
	K3sVersion string `json:"k3sVersion,omitempty"`
	Port       int    `json:"port"`
	KinePort   int    `json:"kinePort"`
	DataDir    string `json:"dataDir"`
	Hostname   string `json:"hostname,omitempty"`
	K3sUnit    string `json:"k3sUnit"`
	KineUnit   string `json:"kineUnit"`
	Invocation string `json:"invocation,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
}

type LinkRecord struct {
	Slot     string `json:"slot"`
	Dumbpipe string `json:"dumbpipe,omitempty"`
	Port     int    `json:"port,omitempty"`
}

func (h *UnitsHost) lock(key string) func() {
	h.mu.Lock()
	l, ok := h.keys[key]
	if !ok {
		l = &sync.Mutex{}
		h.keys[key] = l
	}
	h.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (h *UnitsHost) SlotState(key string) SlotState {
	cp, err := h.readControlPlane(key)
	if err != nil || cp.ActiveSlot == "" {
		return SlotState{Slot: ActiveSlot}
	}
	r := cp.Slots[cp.ActiveSlot]
	return SlotState{Slot: cp.ActiveSlot, K3s: r.K3s, Kine: r.Kine}
}

func (h *UnitsHost) ControlPlane(key string) (ControlPlane, error) { return h.readControlPlane(key) }

type ControlPlaneFront struct {
	Address    string `json:"address"`
	Backend    string `json:"backend"`
	Generation int64  `json:"generation"`
	Control    string `json:"control"`
}

type ControlPlaneMember struct {
	Name         string `json:"name"`
	Unit         string `json:"unit"`
	Object       string `json:"object"`
	InvocationID string `json:"invocationID"`
}

func NewUnitsHost(c UnitsConfig) (*UnitsHost, error) {
	if c.RestConfig == nil || c.Host == "" {
		return nil, errors.New("clusterprovider: the units host needs the kcp the HostProcess supervisor watches")
	}
	if c.Prefix == "" {
		return nil, errors.New("clusterprovider: the units host needs the unit prefix its supervisor places under")
	}
	if c.StateDir == "" {
		return nil, errors.New("clusterprovider: the units host needs the state dir the cluster data dirs live in")
	}
	if c.Workspace == "" {
		c.Workspace = hostprocess.DefaultWorkspace
	}
	if c.Resolve == nil {
		c.Resolve = exec.LookPath
	}
	if c.PlaceTimeout == 0 {
		c.PlaceTimeout = DefaultPlaceTimeout
	}
	if c.StoreTimeout == 0 {
		c.StoreTimeout = DefaultStoreTimeout
	}
	if c.ReapTimeout == 0 {
		c.ReapTimeout = DefaultReapTimeout
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	cfg := rest.CopyConfig(c.RestConfig)
	cfg.Host = strings.TrimSuffix(c.Host, "/") + "/clusters/" + c.Workspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &UnitsHost{c: c, hps: dc.Resource(hostprocess.GVR), pgs: dc.Resource(postgresctl.GVR), obs: dc.Resource(openbaoctl.GVR),
		stores: map[string]Store{}, keys: map[string]*sync.Mutex{}}, nil
}

func slotOr(slot string) string {
	if slot == "" {
		return ActiveSlot
	}
	return slot
}

func StepMember(st procgroup.Step) string { return SlotMember(st.Name, st.Slot) }

func SlotMember(c procgroup.Component, slot string) string {
	switch c {
	case procgroup.ComponentKine:
		return "kine@" + slotOr(slot)
	case procgroup.ComponentK3s:
		return "k3s@" + slotOr(slot)
	case procgroup.ComponentApiserverLink, procgroup.ComponentOpenBaoLink:
		if slot != "" && slot != ActiveSlot {
			return MemberName(c) + "@" + slot
		}
	}
	return MemberName(c)
}

func MemberName(c procgroup.Component) string {
	switch c {
	case procgroup.ComponentKine:
		return "kine@" + ActiveSlot
	case procgroup.ComponentK3s:
		return "k3s@" + ActiveSlot
	case procgroup.ComponentApiserverFront:
		return "front"
	case procgroup.ComponentOpenBao:
		return "bao"
	case procgroup.ComponentApiserverLink:
		return "link-apiserver"
	case procgroup.ComponentOpenBaoLink:
		return "link-openbao"
	case procgroup.ComponentCloudControllerManager:
		return "ccm"
	case procgroup.ComponentExternalSecrets:
		return "eso"
	case procgroup.ComponentExternalSecretsWebhook:
		return "esowh"
	}
	return string(c)
}

func (h *UnitsHost) Unit(key, member string) string {
	return h.c.Prefix + "c-" + key + "-" + member + ".service"
}

func memberLabel(member string) string { return strings.ReplaceAll(member, "@", ".") }

func labelMember(label string) string { return strings.Replace(label, ".", "@", 1) }

func ObjectName(key, member string) string {
	return "c-" + key + "-" + strings.ReplaceAll(member, "@", "-")
}

func (h *UnitsHost) Slice(key string) string {
	return h.c.Prefix + "cluster-" + strings.ReplaceAll(key, "-", "_") + ".slice"
}

func StoreName(key string) string {
	if len(key) <= 32 {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return strings.TrimRight(key[:23], "-") + "-" + hex.EncodeToString(sum[:])[:8]
}

func (h *UnitsHost) StoreUnit(key string) string {
	return h.c.Prefix + postgresctl.ChildPrefix + StoreName(key) + ".service"
}

func decodeObj[T any](u *unstructured.Unstructured) (T, error) {
	var o T
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &o)
	return o, err
}

func encodeObj(o any, kind string) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(kind)
	return u, nil
}

func (h *UnitsHost) StartStore(ctx context.Context, key string) error {
	name := StoreName(key)
	cur, err := h.pgs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		o := v1alpha1.Postgres{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelCluster: key}},
			Spec: v1alpha1.PostgresSpec{Databases: []string{PostgresDatabase, PostgresVaultDatabase}}}
		u, err := encodeObj(&o, "Postgres")
		if err != nil {
			return err
		}
		if _, err := h.pgs.Create(ctx, u, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("clusterprovider: creating the Postgres object %s of %s: %w", name, key, err)
		}
		h.c.Log.Info("cluster provider: the cluster's store is a Postgres object", "cluster", key, "postgres", name,
			"unit", h.StoreUnit(key))
	} else if err != nil {
		return err
	} else if cur.GetDeletionTimestamp() != nil {
		return fmt.Errorf("clusterprovider: the Postgres object %s of %s is being deleted", name, key)
	}
	if _, cached := h.Store(key); !cached {
		if u, err := h.pgs.Get(ctx, name, metav1.GetOptions{}); err == nil {
			if o, derr := decodeObj[v1alpha1.Postgres](u); derr == nil && o.Status.Phase == v1alpha1.PostgresPhaseReady && o.Status.SocketDir != "" {
				h.mu.Lock()
				h.stores[key] = Store{Object: name, Unit: h.StoreUnit(key), SocketDir: o.Status.SocketDir, User: o.Status.User}
				h.mu.Unlock()
			}
		}
	}
	return nil
}

func (h *UnitsHost) EnsureStore(ctx context.Context, key string) (Store, error) {
	name := StoreName(key)
	if err := h.StartStore(ctx, key); err != nil {
		return Store{}, err
	}
	deadline := time.Now().Add(h.c.StoreTimeout)
	for {
		u, err := h.pgs.Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			o, derr := decodeObj[v1alpha1.Postgres](u)
			if derr != nil {
				return Store{}, derr
			}
			if o.Status.Phase == v1alpha1.PostgresPhaseReady && o.Status.SocketDir != "" {
				s := Store{Object: name, Unit: h.StoreUnit(key), SocketDir: o.Status.SocketDir, User: o.Status.User}
				h.mu.Lock()
				h.stores[key] = s
				h.mu.Unlock()
				return s, nil
			}
			if (o.Status.Phase == v1alpha1.PostgresPhaseFailed && o.Status.ObservedGeneration == o.Generation) || time.Now().After(deadline) {
				return Store{}, fmt.Errorf("%w: the Postgres object %s of %s is %s: %s", procgroup.ErrPostgresUnavailable,
					name, key, o.Status.Phase, o.Status.Message)
			}
		} else if time.Now().After(deadline) {
			return Store{}, err
		}
		select {
		case <-ctx.Done():
			return Store{}, ctx.Err()
		case <-time.After(unitPoll):
		}
	}
}

func (h *UnitsHost) Store(key string) (Store, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.stores[key]
	return s, ok
}

func (h *UnitsHost) storeReady(ctx context.Context, key string) bool {
	u, err := h.pgs.Get(ctx, StoreName(key), metav1.GetOptions{})
	if err != nil {
		return false
	}
	o, err := decodeObj[v1alpha1.Postgres](u)
	return err == nil && o.Status.Phase == v1alpha1.PostgresPhaseReady
}

func (h *UnitsHost) DeleteStore(ctx context.Context, key string) error {
	h.mu.Lock()
	delete(h.stores, key)
	h.mu.Unlock()
	name := StoreName(key)
	cur, err := h.pgs.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedField(cur.Object, true, "spec", "deleteData"); err != nil {
		return err
	}
	if _, err := h.pgs.Update(ctx, cur, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	err = h.pgs.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (h *UnitsHost) StoreRemoval(ctx context.Context, key string) (bool, string, error) {
	u, err := h.pgs.Get(ctx, StoreName(key), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	o, err := decodeObj[v1alpha1.Postgres](u)
	if err != nil {
		return false, "", err
	}
	return false, fmt.Sprintf("the Postgres object %s is %s: %s", StoreName(key), o.Status.Phase, o.Status.Message), nil
}

func (h *UnitsHost) PurgeData(key string) error {
	dir, err := ClusterDataDir(h.c.StateDir, key)
	if err != nil {
		return err
	}
	dirs := []string{dir}
	if key != "" && filepath.Base(key) == key && key != "." && key != ".." {
		dirs = append(dirs, filepath.Join(h.c.StateDir, key))
	}
	if h.c.VaultStateDir != "" {
		dirs = append(dirs, openbaoctl.ServerDir(h.c.VaultStateDir, h.VaultServerName(key)))
	}
	for _, d := range dirs {
		if _, err := os.Lstat(d); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return fmt.Errorf("clusterprovider: purging %s of deleted cluster %s: %w", d, key, err)
		}
		h.c.Log.Info("cluster provider: identity bundle purged", "cluster", key, "dir", d)
	}
	return nil
}

func (h *UnitsHost) Members(ctx context.Context, key string) ([]v1alpha1.HostProcess, error) {
	list, err := h.hps.List(ctx, metav1.ListOptions{LabelSelector: LabelCluster + "=" + key})
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.HostProcess, 0, len(list.Items))
	for i := range list.Items {
		o, err := decodeObj[v1alpha1.HostProcess](&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (h *UnitsHost) gateAlive(ctx context.Context, key, member, inv string, gate func(context.Context) error) error {
	gctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go func() {
		for {
			select {
			case <-gctx.Done():
				return
			case <-time.After(unitPoll):
			}
			o, err := h.member(gctx, key, member)
			if err != nil {
				continue
			}
			if dead := memberDead(o, inv); dead != "" {
				cancel(fmt.Errorf("clusterprovider: %s of %s %s", member, key, dead))
				return
			}
		}
	}()
	err := gate(gctx)
	if cause := context.Cause(gctx); err != nil && cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("%w; the gate saw: %v", cause, err)
	}
	return err
}

func memberDead(o *v1alpha1.HostProcess, inv string) string {
	if o == nil {
		return "is gone"
	}
	switch o.Status.Phase {
	case v1alpha1.HostProcessPhaseFailed, v1alpha1.HostProcessPhaseLost, v1alpha1.HostProcessPhaseStopped:
		return fmt.Sprintf("is %s %s during its readiness gate: %s", o.Status.Phase, o.Status.LostReason, o.Status.Message)
	}
	if rt := o.Status.Runtime; rt != nil && inv != "" && rt.InvocationID != "" && rt.InvocationID != inv {
		return fmt.Sprintf("restarted during its readiness gate (invocation %s, now %s): %s", inv, rt.InvocationID, o.Status.Message)
	}
	return ""
}

func (h *UnitsHost) member(ctx context.Context, key, member string) (*v1alpha1.HostProcess, error) {
	u, err := h.hps.Get(ctx, ObjectName(key, member), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o, err := decodeObj[v1alpha1.HostProcess](u)
	return &o, err
}

func Verified(o *v1alpha1.HostProcess) bool {
	if o == nil || o.DeletionTimestamp != nil || o.Status.Phase != v1alpha1.HostProcessPhaseRunning || o.Status.Runtime == nil {
		return false
	}
	inv := o.Status.Runtime.InvocationID
	return inv != "" && o.Annotations[AnnotationVerified] == inv
}

func (h *UnitsHost) Live(key string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := h.Members(ctx, key)
	return err == nil && len(m) > 0
}

func (h *UnitsHost) Unhealthy(ctx context.Context, key string) ([]string, error) {
	members, err := h.Members(ctx, key)
	if err != nil {
		return nil, err
	}
	have := map[string]*v1alpha1.HostProcess{}
	for i := range members {
		have[labelMember(members[i].Labels[LabelMember])] = &members[i]
	}
	var sick []string
	cp, err := h.readControlPlane(key)
	if err != nil && len(members) > 0 {
		sick = append(sick, ControlPlaneFile+": "+err.Error())
	}
	vault := MemberName(procgroup.ComponentOpenBao)
	recorded := map[string]bool{}
	for _, m := range cp.Members {
		recorded[m.Name] = true
		if m.Name == vault {
			if why := h.vaultSick(ctx, key); why != "" {
				sick = append(sick, m.Name+": "+why)
			}
			continue
		}
		if have[m.Name] == nil {
			sick = append(sick, m.Name+": no HostProcess")
		}
	}
	for _, o := range members {
		name := labelMember(o.Labels[LabelMember])
		if len(recorded) > 0 && !recorded[name] {
			continue
		}
		switch {
		case o.DeletionTimestamp != nil:
			sick = append(sick, name+": stopping")
		case o.Status.Phase != v1alpha1.HostProcessPhaseRunning:
			sick = append(sick, strings.TrimSpace(fmt.Sprintf("%s: %s %s %s", name, o.Status.Phase, o.Status.LostReason, o.Status.Message)))
		case !Verified(&o):
			sick = append(sick, fmt.Sprintf("%s: invocation %s is not verified since its restart", name, o.Status.Runtime.InvocationID))
		}
	}
	slices.Sort(sick)
	return sick, nil
}

func (h *UnitsHost) Adoption(ctx context.Context, key string) ([]string, error) {
	members, err := h.Members(ctx, key)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, o := range members {
		inv := ""
		if o.Status.Runtime != nil {
			inv = o.Status.Runtime.InvocationID
		}
		out = append(out, fmt.Sprintf("%s=%s invocation=%s verified=%t", labelMember(o.Labels[LabelMember]), o.Status.Phase, inv, Verified(&o)))
	}
	slices.Sort(out)
	return out, nil
}

func memberDeps(h *UnitsHost, key string, c procgroup.Component, slot string) []string {
	switch c {
	case procgroup.ComponentApiserverFront:
		return nil
	case procgroup.ComponentKine, procgroup.ComponentOpenBao:
		return []string{h.StoreUnit(key)}
	case procgroup.ComponentK3s:
		return []string{h.Unit(key, SlotMember(procgroup.ComponentKine, slot))}
	case procgroup.ComponentOpenBaoLink:
		return []string{h.Unit(key, MemberName(procgroup.ComponentOpenBao))}
	}
	return []string{h.Unit(key, MemberName(procgroup.ComponentApiserverFront))}
}

func (h *UnitsHost) memberSpec(key string, st procgroup.Step, spec procgroup.Spec) (v1alpha1.HostProcessSpec, error) {
	c := st.Name
	if len(spec.Argv) == 0 {
		return v1alpha1.HostProcessSpec{}, fmt.Errorf("clusterprovider: %s of %s has no argv", c, key)
	}
	exe := spec.Argv[0]
	if !filepath.IsAbs(exe) {
		p, err := h.c.Resolve(exe)
		if err != nil {
			return v1alpha1.HostProcessSpec{}, fmt.Errorf("clusterprovider: %s of %s runs %s, which is not on this host: %w", c, key, exe, err)
		}
		if exe, err = filepath.Abs(p); err != nil {
			return v1alpha1.HostProcessSpec{}, err
		}
	}
	deps := memberDeps(h, key, c, st.Slot)
	out := v1alpha1.HostProcessSpec{Unit: h.Unit(key, StepMember(st)), Exec: exe, Argv: append([]string(nil), spec.Argv[1:]...),
		EnvFiles: append([]string(nil), spec.EnvFiles...), RestartPolicy: v1alpha1.HostProcessRestartOnFailure,
		Stop:  &v1alpha1.HostProcessStop{Signal: "SIGTERM", GraceSeconds: memberStopGrace},
		After: deps, Wants: deps, Slice: h.Slice(key), Placement: v1alpha1.HostProcessPlacementSupervisor}
	switch c {
	case procgroup.ComponentApiserverLink, procgroup.ComponentOpenBaoLink:
		out.RestartPolicy = v1alpha1.HostProcessRestartAlways
	case procgroup.ComponentApiserverFront:
		out.RestartPolicy = v1alpha1.HostProcessRestartAlways
		out.FdStore = FrontFDStore
		out.Stop.GraceSeconds = frontStopGrace
	}
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		out.Env = append(out.Env, v1alpha1.HostProcessEnv{Name: k, Value: v})
	}
	if spec.Log != "" {
		out.Output = &v1alpha1.HostProcessOutput{Stdout: spec.Log, Stderr: spec.Log, CapBytes: MemberOutputCap}
	}
	return out, nil
}

func (h *UnitsHost) remove(ctx context.Context, name string) error {
	for i := 0; i < 5; i++ {
		u, err := h.hps.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		fin := slices.DeleteFunc(u.GetFinalizers(), func(f string) bool { return f == MemberFinalizer })
		if len(fin) != len(u.GetFinalizers()) {
			u.SetFinalizers(fin)
			if _, err := h.hps.Update(ctx, u, metav1.UpdateOptions{}); err != nil {
				if apierrors.IsConflict(err) {
					continue
				}
				return err
			}
		}
		if u.GetDeletionTimestamp() == nil {
			if err := h.hps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := h.hps.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			return nil
		}
		time.Sleep(unitPoll)
	}
	return fmt.Errorf("clusterprovider: the HostProcess %s was not removed", name)
}

func rotateLog(path string) {
	if path == "" {
		return
	}
	if _, err := os.Stat(path); err == nil {
		os.Rename(path, path+".prev")
	}
}

func (h *UnitsHost) place(ctx context.Context, key, member string, cur *v1alpha1.HostProcess, want v1alpha1.HostProcessSpec) (*v1alpha1.HostProcess, error) {
	name := ObjectName(key, member)
	if cur != nil {
		switch {
		case cur.DeletionTimestamp != nil, cur.Status.Phase == v1alpha1.HostProcessPhaseLost, cur.Status.Phase == v1alpha1.HostProcessPhaseStopped:
			h.c.Log.Warn("cluster provider: a member is not running; it is placed again", "cluster", key, "member", member,
				"phase", cur.Status.Phase, "lostReason", cur.Status.LostReason, "message", cur.Status.Message)
			if err := h.remove(ctx, name); err != nil {
				return nil, err
			}
			cur = nil
		case !reflect.DeepEqual(cur.Spec, want):
			cur.Spec = want
			u, err := encodeObj(cur, "HostProcess")
			if err != nil {
				return nil, err
			}
			if _, err := h.hps.Update(ctx, u, metav1.UpdateOptions{}); err != nil {
				return nil, err
			}
			h.c.Log.Info("cluster provider: a member's spec changed; the running unit keeps the old one until it restarts",
				"cluster", key, "member", member)
		}
	}
	if cur == nil {
		if want.Output != nil {
			rotateLog(want.Output.Stdout)
		}
		o := v1alpha1.HostProcess{ObjectMeta: metav1.ObjectMeta{Name: name,
			Labels:     map[string]string{LabelCluster: key, LabelMember: memberLabel(member)},
			Finalizers: []string{MemberFinalizer}}, Spec: want}
		u, err := encodeObj(&o, "HostProcess")
		if err != nil {
			return nil, err
		}
		if _, err := h.hps.Create(ctx, u, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("clusterprovider: creating the HostProcess of %s of %s: %w", member, key, err)
		}
		h.c.Log.Info("cluster provider: member placed", "cluster", key, "member", member, "unit", want.Unit, "slice", want.Slice,
			"after", want.After)
	}
	deadline := time.Now().Add(h.c.PlaceTimeout)
	for {
		o, err := h.member(ctx, key, member)
		if err != nil {
			return nil, err
		}
		if o != nil {
			switch o.Status.Phase {
			case v1alpha1.HostProcessPhaseRunning:
				if o.Status.Runtime != nil && o.Status.Runtime.InvocationID != "" {
					return o, nil
				}
			case v1alpha1.HostProcessPhaseLost:
				return nil, fmt.Errorf("%w: %s of %s is Lost %s: %s", ErrMemberLost, member, key, o.Status.LostReason, o.Status.Message)
			case v1alpha1.HostProcessPhaseFailed:
				return nil, fmt.Errorf("clusterprovider: %s of %s is Failed: %s", member, key, o.Status.Message)
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("clusterprovider: %s of %s is %s after %s: %s", member, key, o.Status.Phase, h.c.PlaceTimeout, o.Status.Message)
			}
		} else if time.Now().After(deadline) {
			return nil, fmt.Errorf("clusterprovider: the HostProcess of %s of %s vanished while it was placed", member, key)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(placePoll):
		}
	}
}

func (h *UnitsHost) verify(ctx context.Context, key, member, inv string) error {
	for i := 0; i < 5; i++ {
		u, err := h.hps.Get(ctx, ObjectName(key, member), metav1.GetOptions{})
		if err != nil {
			return err
		}
		ann := u.GetAnnotations()
		if ann == nil {
			ann = map[string]string{}
		}
		ann[AnnotationVerified] = inv
		u.SetAnnotations(ann)
		if _, err = h.hps.Update(ctx, u, metav1.UpdateOptions{}); err == nil || !apierrors.IsConflict(err) {
			return err
		}
	}
	return fmt.Errorf("clusterprovider: recording the verified invocation of %s of %s kept conflicting", member, key)
}

type unitMember struct{ path string }

func (m unitMember) Output() string {
	f, err := os.Open(m.path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > memberOutputTail {
		f.Seek(st.Size()-memberOutputTail, io.SeekStart)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

func (h *UnitsHost) Spawn(ctx context.Context, key string, steps []procgroup.Step) error {
	store, ok := h.Store(key)
	if !ok {
		var err error
		if store, err = h.EnsureStore(ctx, key); err != nil {
			return err
		}
	}
	unlock := h.lock(key)
	defer unlock()
	prev, _ := h.readControlPlane(key)
	cp := ControlPlane{ActiveSlot: ActiveSlot, Slice: h.Slice(key), Store: store.Unit, Front: prev.Front, Slots: prev.Slots,
		Links: prev.Links, LastRoll: prev.LastRoll}
	for _, st := range steps {
		member := StepMember(st)
		if st.Name == procgroup.ComponentK3s {
			cp.ActiveSlot = slotOr(st.Slot)
		}
		cp.Members = append(cp.Members, ControlPlaneMember{Name: member, Unit: h.Unit(key, member), Object: ObjectName(key, member)})
	}
	if err := h.writeControlPlane(key, cp); err != nil {
		return err
	}
	var cpMu sync.Mutex
	run := func(ctx context.Context, i int, st procgroup.Step) error {
		if (st.Name == procgroup.ComponentKine || st.Name == procgroup.ComponentOpenBao) && !h.storeReady(ctx, key) {
			return fmt.Errorf("%w (cluster %s, Postgres %s)", procgroup.ErrPostgresUnavailable, key, store.Object)
		}
		if st.Name == procgroup.ComponentOpenBao {
			o, err := h.placeVault(ctx, key, st)
			if err != nil {
				return fmt.Errorf("clusterprovider: the vault of %s: %w", key, err)
			}
			if st.Ready != nil {
				if err := st.Ready(ctx, unitMember{path: st.Spec.Log}); err != nil {
					return fmt.Errorf("clusterprovider: the vault of %s never became ready: %w", key, err)
				}
			}
			cpMu.Lock()
			if o.Status.Runtime != nil {
				cp.Members[i].InvocationID = o.Status.Runtime.Instance
			}
			cp.Members[i].Object = o.Name
			cpMu.Unlock()
			h.c.Log.Info("cluster provider: vault ready and synced to its declaration", "cluster", key, "openbaoserver", o.Name,
				"unit", o.Spec.Unit, "address", o.Status.Address)
			return nil
		}
		inv, err := h.runStep(ctx, key, st)
		if err != nil {
			return err
		}
		cpMu.Lock()
		cp.Members[i].InvocationID = inv
		cpMu.Unlock()
		return nil
	}
	if err := runGraph(ctx, steps, run); err != nil {
		return err
	}
	if f, err := h.frontRecord(ctx, key); err == nil {
		cp.Front = f
	}
	return h.writeControlPlane(key, cp)
}

func stepNeeds(steps []procgroup.Step, i int) []int {
	index := map[procgroup.Component]int{}
	for j, st := range steps {
		if _, seen := index[st.Name]; !seen {
			index[st.Name] = j
		}
	}
	var names []procgroup.Component
	switch steps[i].Name {
	case procgroup.ComponentApiserverFront:
	case procgroup.ComponentOpenBao, procgroup.ComponentKine:
		names = []procgroup.Component{procgroup.ComponentApiserverFront}
	case procgroup.ComponentK3s:
		names = []procgroup.Component{procgroup.ComponentApiserverFront, procgroup.ComponentKine, procgroup.ComponentOpenBao}
	case procgroup.ComponentCloudControllerManager, procgroup.ComponentExternalSecretsWebhook:
		names = []procgroup.Component{procgroup.ComponentK3s}
	case procgroup.ComponentExternalSecrets:
		names = []procgroup.Component{procgroup.ComponentK3s, procgroup.ComponentOpenBao}
	case procgroup.ComponentApiserverLink:
		names = []procgroup.Component{procgroup.ComponentApiserverFront, procgroup.ComponentK3s}
	case procgroup.ComponentOpenBaoLink:
		names = []procgroup.Component{procgroup.ComponentOpenBao}
	default:
		out := make([]int, 0, i)
		for j := 0; j < i; j++ {
			out = append(out, j)
		}
		return out
	}
	out := make([]int, 0, len(names))
	for _, n := range names {
		if j, ok := index[n]; ok && j != i {
			out = append(out, j)
		}
	}
	return out
}

func runGraph(ctx context.Context, steps []procgroup.Step, run func(context.Context, int, procgroup.Step) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make([]chan struct{}, len(steps))
	for i := range done {
		done[i] = make(chan struct{})
	}
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}
	for i, st := range steps {
		wg.Add(1)
		go func(i int, st procgroup.Step) {
			defer wg.Done()
			for _, j := range stepNeeds(steps, i) {
				select {
				case <-done[j]:
				case <-ctx.Done():
					return
				}
			}
			if err := run(ctx, i, st); err != nil {
				fail(err)
				return
			}
			close(done[i])
		}(i, st)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (h *UnitsHost) runStep(ctx context.Context, key string, st procgroup.Step) (string, error) {
	member := StepMember(st)
	cur, err := h.member(ctx, key, member)
	if err != nil {
		return "", err
	}
	if Verified(cur) {
		h.c.Log.Info("cluster provider: member adopted", "cluster", key, "member", member, "unit", cur.Spec.Unit,
			"invocation", cur.Status.Runtime.InvocationID)
		return cur.Status.Runtime.InvocationID, nil
	}
	spec := st.Spec
	if st.Prepare != nil {
		if spec, err = st.Prepare(ctx, spec); err != nil {
			return "", fmt.Errorf("clusterprovider: preparing %s of %s: %w", member, key, err)
		}
	}
	want, err := h.memberSpec(key, st, spec)
	if err != nil {
		return "", err
	}
	o, err := h.place(ctx, key, member, cur, want)
	if err != nil {
		return "", err
	}
	if st.Ready == nil {
		return "", fmt.Errorf("clusterprovider: %s of %s has no readiness gate", member, key)
	}
	if err := h.gateAlive(ctx, key, member, o.Status.Runtime.InvocationID, func(ctx context.Context) error {
		return st.Ready(ctx, unitMember{path: spec.Log})
	}); err != nil {
		return "", fmt.Errorf("clusterprovider: %s of %s never became ready: %w", member, key, err)
	}
	inv := o.Status.Runtime.InvocationID
	if err := h.verify(ctx, key, member, inv); err != nil {
		return "", err
	}
	h.c.Log.Info("cluster provider: member ready", "cluster", key, "member", member, "unit", o.Spec.Unit, "invocation", inv)
	return inv, nil
}

func (h *UnitsHost) EnsureFront(ctx context.Context, key string, st procgroup.Step) error {
	if st.Name != procgroup.ComponentApiserverFront {
		return fmt.Errorf("clusterprovider: EnsureFront was handed %s, not the apiserver front", st.Name)
	}
	unlock := h.lock(key)
	defer unlock()
	_, err := h.runStep(ctx, key, st)
	return err
}

func (h *UnitsHost) FrontControl(key string) (string, error) {
	dir, err := ClusterDataDir(h.c.StateDir, key)
	if err != nil {
		return "", err
	}
	return FrontControlPath(h.c.StateDir, dir, key), nil
}

func (h *UnitsHost) frontRecord(ctx context.Context, key string) (*ControlPlaneFront, error) {
	ctl, err := h.FrontControl(key)
	if err != nil {
		return nil, err
	}
	st, err := (apiserverfront.Client{Path: ctl}).Status(ctx)
	if err != nil {
		return nil, err
	}
	return &ControlPlaneFront{Address: st.Address, Backend: st.Backend, Generation: st.Generation, Control: ctl}, nil
}

func (h *UnitsHost) controlPlanePath(key string) (string, error) {
	dir, err := ClusterDataDir(h.c.StateDir, key)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ControlPlaneFile), nil
}

func (h *UnitsHost) readControlPlane(key string) (ControlPlane, error) {
	var cp ControlPlane
	p, err := h.controlPlanePath(key)
	if err != nil {
		return cp, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return cp, err
	}
	return cp, json.Unmarshal(b, &cp)
}

func (h *UnitsHost) writeControlPlane(key string, cp ControlPlane) error {
	p, err := h.controlPlanePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	cp.WrittenAt = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (h *UnitsHost) Reap(ctx context.Context, key string) error {
	unlock := h.lock(key)
	defer unlock()
	members, err := h.Members(ctx, key)
	if err != nil {
		return err
	}
	for _, o := range members {
		if o.DeletionTimestamp != nil {
			continue
		}
		if err := h.hps.Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if err := h.reapVault(ctx, key); err != nil {
		return err
	}
	if len(members) == 0 {
		return nil
	}
	h.c.Log.Info("cluster provider: the cluster's slice is stopped; waiting for every member to be inactive",
		"cluster", key, "slice", h.Slice(key), "members", len(members))
	deadline := time.Now().Add(h.c.ReapTimeout)
	for {
		members, err = h.Members(ctx, key)
		if err != nil {
			return err
		}
		pending := 0
		for _, o := range members {
			switch o.Status.Phase {
			case v1alpha1.HostProcessPhaseStopped, v1alpha1.HostProcessPhaseLost, v1alpha1.HostProcessPhaseFailed:
				if err := h.remove(ctx, o.Name); err != nil {
					return err
				}
			default:
				pending++
			}
		}
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("clusterprovider: %d members of %s are still active after %s", pending, key, h.c.ReapTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(unitPoll):
		}
	}
	if p, err := h.controlPlanePath(key); err == nil {
		os.Remove(p)
	}
	h.c.Log.Info("cluster provider: every member of the cluster is stopped", "cluster", key, "slice", h.Slice(key))
	return nil
}

func (h *UnitsHost) LinkState(key string, service v1alpha1.ClusterLinkService) LinkRecord {
	cp, err := h.readControlPlane(key)
	if err != nil {
		return LinkRecord{Slot: ActiveSlot}
	}
	if r, ok := cp.Links[string(service)]; ok && r.Slot != "" {
		return r
	}
	return LinkRecord{Slot: ActiveSlot}
}
