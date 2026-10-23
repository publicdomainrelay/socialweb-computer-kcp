package clusterprovider

import (
	"bytes"
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
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/apiserverfront"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterlink"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/clusterstack"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/materialiser"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/portallocator"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/procgroup"
)

const (
	RollRequestFile = "roll-request.json"
	RollsDirName    = "rolls"
	BackupsDirName  = "backups"

	RollKindCluster = "cluster"
	RollKindLink    = "link"

	DefaultIdleBound    = 30 * time.Minute
	DefaultRollWatch    = 60 * time.Second
	DefaultRollInterval = 5 * time.Second
	idlePoll            = 10 * time.Second
	drainWait           = 5 * time.Second
	kineSchemaMark      = "tables and indexes are up to date"
)

type RollRequest struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	K3s         string    `json:"k3s,omitempty"`
	Kine        string    `json:"kine,omitempty"`
	Dumbpipe    string    `json:"dumbpipe,omitempty"`
	Service     string    `json:"service,omitempty"`
	IdleBound   string    `json:"idleBound,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
}

type LeaseHandover struct {
	Lease   string  `json:"lease"`
	From    string  `json:"from"`
	To      string  `json:"to,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

type RollReport struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Cluster string `json:"cluster"`
	From    string `json:"from"`
	To      string `json:"to"`

	FromVersion string   `json:"fromVersion,omitempty"`
	ToVersion   string   `json:"toVersion,omitempty"`
	Backup      string   `json:"backup,omitempty"`
	Relet       []string `json:"relet,omitempty"`

	Nodes        int     `json:"nodes"`
	IdleSeconds  float64 `json:"idleSeconds"`
	IdleDeferred bool    `json:"idleDeferred,omitempty"`

	StartedAt     time.Time `json:"startedAt"`
	GreenReadyAt  time.Time `json:"greenReadyAt,omitzero"`
	PersistedAt   time.Time `json:"persistedAt,omitzero"`
	FlippedAt     time.Time `json:"flippedAt,omitzero"`
	OldStoppedAt  time.Time `json:"oldStoppedAt,omitzero"`
	DoneAt        time.Time `json:"doneAt,omitzero"`
	StartToReady  float64   `json:"startToReadySeconds,omitempty"`
	OldStopSecs   float64   `json:"oldStopSeconds,omitempty"`
	OpenOnOld     int       `json:"openOnOldAtStop"`
	Drained       int       `json:"drained"`
	FrontBackend  string    `json:"frontBackend,omitempty"`
	Endpoints     []string  `json:"endpoints,omitempty"`
	EndpointsLost int       `json:"endpointsNotAdvertisedPolls"`

	Leases          []LeaseHandover `json:"leases,omitempty"`
	NodeTransitions []string        `json:"nodeReadyTransitions,omitempty"`

	Ticket       string `json:"ticket,omitempty"`
	TicketBefore string `json:"ticketBefore,omitempty"`
	OldListener  string `json:"oldListener,omitempty"`
	NewListener  string `json:"newListener,omitempty"`

	Error string `json:"error,omitempty"`
}

func secsBetween(a, b time.Time) float64 { return float64(b.Sub(a).Milliseconds()) / 1000 }

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func RollRequestPath(stateDir, cluster string) (string, error) {
	dir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, RollRequestFile), nil
}

func RollReportPath(stateDir, cluster, id string) (string, error) {
	dir, err := ClusterDataDir(stateDir, cluster)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, RollsDirName, id+".json"), nil
}

func WriteRollRequest(stateDir, cluster string, r RollRequest) (string, error) {
	p, err := RollRequestPath(stateDir, cluster)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Dir(p)); err != nil {
		return "", fmt.Errorf("clusterprovider: cluster %s has no data dir under %s: %w", cluster, stateDir, err)
	}
	if _, err := os.Stat(p); err == nil {
		return "", fmt.Errorf("clusterprovider: %s already holds a roll request; the provider has not taken it yet", p)
	}
	return p, writeJSONFile(p, r)
}

func ReadRollReport(stateDir, cluster, id string) (RollReport, bool, error) {
	p, err := RollReportPath(stateDir, cluster, id)
	if err != nil {
		return RollReport{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return RollReport{}, false, nil
	}
	if err != nil {
		return RollReport{}, false, err
	}
	var r RollReport
	return r, true, json.Unmarshal(b, &r)
}

type Roller struct {
	Units *UnitsHost

	Stack StackConfig

	Clusters func(ctx context.Context) ([]string, error)

	Ready func(ctx context.Context, cluster string) (bool, error)

	Leases func(cluster string) (materialiser.Leases, error)

	Relet func(cluster string, p portallocator.Purpose) (portallocator.Lease, bool, error)

	Interval time.Duration

	Name string

	Log *slog.Logger

	status RollerStatus
}

func (r *Roller) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Roller) Run(ctx context.Context) {
	every := r.Interval
	if every <= 0 {
		every = DefaultRollInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		keys, err := r.Clusters(ctx)
		if err == nil {
			for _, key := range keys {
				if perr := r.pass(ctx, key); perr != nil && err == nil {
					err = perr
				}
			}
		}
		r.noteStatus(keys, err)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Roller) pass(ctx context.Context, key string) error {
	p, err := RollRequestPath(r.Stack.StateDir, key)
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var req RollRequest
	if err := json.Unmarshal(b, &req); err != nil || req.ID == "" {
		r.log().Error("cluster provider: an unreadable roll request is dropped", "cluster", key, "path", p)
		os.Remove(p)
		return nil
	}
	ready, rerr := r.Ready(ctx, key)
	if !ready {
		r.noteObserved(key, req, RollStateWaiting, rerr)
		if rerr != nil {
			r.log().Warn("cluster provider: a roll waits; the cluster's readiness could not be read", "cluster", key, "roll", req.ID, "err", rerr.Error())
			return rerr
		}
		r.log().Info("cluster provider: a roll waits for its cluster to be Ready", "cluster", key, "roll", req.ID)
		return nil
	}
	r.noteObserved(key, req, RollStateStarted, nil)
	r.log().Info("cluster provider: roll started", "cluster", key, "roll", req.ID, "kind", req.Kind)
	var rep RollReport
	switch req.Kind {
	case RollKindCluster:
		rep = r.RollCluster(ctx, key, req)
	case RollKindLink:
		rep = r.RollLink(ctx, key, req)
	default:
		rep = RollReport{ID: req.ID, Kind: req.Kind, Cluster: key, Error: "unknown roll kind " + req.Kind}
	}
	rep.DoneAt = time.Now().UTC()
	if rp, err := RollReportPath(r.Stack.StateDir, key, req.ID); err == nil {
		if err := writeJSONFile(rp, rep); err != nil {
			r.log().Error("cluster provider: writing the roll report", "cluster", key, "err", err.Error())
		}
	}
	os.Remove(p)
	if op, err := RollObservedPath(r.Stack.StateDir, key, req.ID); err == nil {
		os.Remove(op)
	}
	if rep.Error != "" {
		r.log().Error("cluster provider: roll failed", "cluster", key, "roll", req.ID, "err", rep.Error)
		return nil
	}
	r.log().Info("cluster provider: roll done", "cluster", key, "roll", req.ID, "from", rep.From, "to", rep.To)
	return nil
}

var k3sVersionLine = regexp.MustCompile(`v(\d+)\.(\d+)\.(\d+)\S*`)

func ExecutableVersion(ctx context.Context, exe string) (string, error) {
	out, err := exec.CommandContext(ctx, exe, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("clusterprovider: %s --version: %w", exe, err)
	}
	m := k3sVersionLine.FindString(string(out))
	if m == "" {
		return "", fmt.Errorf("clusterprovider: %s --version printed no version: %q", exe, strings.TrimSpace(string(out)))
	}
	return m, nil
}

func minorOf(v string) (int, error) {
	m := k3sVersionLine.FindStringSubmatch(v)
	if m == nil {
		return 0, fmt.Errorf("clusterprovider: %q is no Kubernetes version", v)
	}
	return strconv.Atoi(m[2])
}

func CheckSkew(active, target string, guests []string) error {
	am, err := minorOf(active)
	if err != nil {
		return err
	}
	tm, err := minorOf(target)
	if err != nil {
		return err
	}
	if tm != am && tm != am+1 {
		return fmt.Errorf("clusterprovider: the target k3s %s is not the active minor %d or the next one; minors move one at a time and a rollback stays within its minor", target, am)
	}
	for _, g := range guests {
		gm, err := minorOf(g)
		if err != nil {
			return err
		}
		if tm < gm || tm > gm+3 {
			return fmt.Errorf("clusterprovider: the target k3s %s is older than a joined guest's %s or more than three minors newer", target, g)
		}
	}
	return nil
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var sharedCAs = []string{"server-ca.crt", "client-ca.crt", "request-header-ca.crt"}

func SameCAs(blueTLS, greenTLS string) error {
	for _, n := range sharedCAs {
		a, err := fileSum(filepath.Join(blueTLS, n))
		if err != nil {
			return err
		}
		b, err := fileSum(filepath.Join(greenTLS, n))
		if err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("clusterprovider: the new slot serves another %s than the active one (%s, %s)", n, a[:12], b[:12])
		}
	}
	return nil
}

func workingPods(ctx context.Context, core kubernetes.Interface) (nodes, running int, guests []string, err error) {
	nl, err := core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, nil, err
	}
	for _, n := range nl.Items {
		guests = append(guests, n.Status.NodeInfo.KubeletVersion)
	}
	if len(nl.Items) == 0 {
		return 0, 0, guests, nil
	}
	pl, err := core.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{FieldSelector: "status.phase=Running"})
	if err != nil {
		return 0, 0, nil, err
	}
	for _, p := range pl.Items {
		if p.Namespace != metav1.NamespaceSystem && p.Spec.NodeName != "" {
			running++
		}
	}
	return len(nl.Items), running, guests, nil
}

func nodeReadySince(ctx context.Context, core kubernetes.Interface) map[string]string {
	out := map[string]string{}
	nl, err := core.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, n := range nl.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == corev1.NodeReady {
				out[n.Name] = string(c.Status) + "@" + c.LastTransitionTime.UTC().Format(time.RFC3339)
			}
		}
	}
	return out
}

func leaseHolders(ctx context.Context, core kubernetes.Interface) map[string]string {
	out := map[string]string{}
	ll, err := core.CoordinationV1().Leases(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out
	}
	for _, l := range ll.Items {
		if strings.HasPrefix(l.Name, "apiserver-") || l.Spec.HolderIdentity == nil {
			continue
		}
		out[l.Name] = *l.Spec.HolderIdentity
	}
	return out
}

func backupStore(ctx context.Context, cfg StackConfig, s Store, dir string) error {
	pgDump, err := cfg.resolveExe("", cfg.PgDumpExec, "pg_dump")
	if err != nil {
		return fmt.Errorf("clusterprovider: a minor bump takes a pg_dump first and none is on this host: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, db := range []string{PostgresDatabase, PostgresVaultDatabase} {
		out := filepath.Join(dir, db+".sql")
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, pgDump, "-h", s.SocketDir, "-U", s.User, "-f", out, db)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("clusterprovider: pg_dump %s: %w: %s", db, err, strings.TrimSpace(stderr.String()))
		}
		if st, err := os.Stat(out); err != nil || st.Size() == 0 {
			return fmt.Errorf("clusterprovider: pg_dump %s wrote nothing", db)
		}
	}
	return nil
}

func (h *UnitsHost) placeGated(ctx context.Context, key string, st procgroup.Step, gate func(context.Context, procgroup.Member) error) (*v1alpha1.HostProcess, error) {
	member := StepMember(st)
	cur, err := h.member(ctx, key, member)
	if err != nil {
		return nil, err
	}
	if cur != nil {
		if err := h.remove(ctx, ObjectName(key, member)); err != nil {
			return nil, err
		}
	}
	want, err := h.memberSpec(key, st, st.Spec)
	if err != nil {
		return nil, err
	}
	o, err := h.place(ctx, key, member, nil, want)
	if err != nil {
		return nil, err
	}
	gctx, cancel := context.WithTimeout(ctx, GateTimeout)
	defer cancel()
	if err := h.gateAlive(gctx, key, member, o.Status.Runtime.InvocationID, func(ctx context.Context) error {
		return gate(ctx, unitMember{path: st.Spec.Log})
	}); err != nil {
		return nil, fmt.Errorf("clusterprovider: %s of %s never became ready: %w", member, key, err)
	}
	inv := o.Status.Runtime.InvocationID
	if err := h.verify(ctx, key, member, inv); err != nil {
		return nil, err
	}
	return o, nil
}

func waitOutput(ctx context.Context, m procgroup.Member, mark, what string) error {
	return poll(ctx, func(context.Context) (bool, error) {
		return strings.Contains(strings.ToLower(m.Output()), mark), nil
	}, func() error { return &errNotReady{what: what, last: tail(m.Output())} })
}

func slotSteps(spec clusterstack.Spec) (kine, k3s procgroup.Step, err error) {
	kargv, err := spec.KineArgv()
	if err != nil {
		return kine, k3s, err
	}
	sargv, err := spec.ServerArgv()
	if err != nil {
		return kine, k3s, err
	}
	klog, err := componentLog(spec.DataDir, procgroup.ComponentKine)
	if err != nil {
		return kine, k3s, err
	}
	k3sLog, err := componentLog(spec.DataDir, procgroup.ComponentK3s)
	if err != nil {
		return kine, k3s, err
	}
	if spec.Slot != ActiveSlot {
		klog = strings.TrimSuffix(klog, ".log") + "@" + spec.Slot + ".log"
		k3sLog = strings.TrimSuffix(k3sLog, ".log") + "@" + spec.Slot + ".log"
	}
	rotateLog(klog)
	rotateLog(k3sLog)
	kine = procgroup.Step{Name: procgroup.ComponentKine, Slot: spec.Slot, Spec: procgroup.Spec{Argv: kargv, Log: klog}}
	k3s = procgroup.Step{Name: procgroup.ComponentK3s, Slot: spec.Slot, Spec: procgroup.Spec{Argv: sargv, Log: k3sLog}}
	return kine, k3s, nil
}

func otherSlot(s string) string {
	if s == "b" {
		return "a"
	}
	return "b"
}

func (r *Roller) RollCluster(ctx context.Context, key string, req RollRequest) (rep RollReport) {
	h := r.Units
	rep = RollReport{ID: req.ID, Kind: RollKindCluster, Cluster: key, StartedAt: time.Now().UTC()}
	fail := func(err error) RollReport {
		rep.Error = err.Error()
		return rep
	}
	unlock := h.lock(key)
	defer unlock()
	cp, err := h.readControlPlane(key)
	if err != nil {
		return fail(err)
	}
	blue := slotOr(cp.ActiveSlot)
	green := otherSlot(blue)
	rep.From, rep.To = blue, green
	blueRec := cp.Slots[blue]
	l, err := r.Leases(key)
	if err != nil {
		return fail(err)
	}
	o := materialiser.Observed{Cluster: v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: key}}}
	blueSpec, err := r.Stack.SlotSpec(o, l, SlotState{Slot: blue, K3s: blueRec.K3s, Kine: blueRec.Kine})
	if err != nil {
		return fail(err)
	}
	gk3s, gkine := req.K3s, req.Kine
	if gk3s == "" {
		gk3s = blueSpec.K3sExecutable()
	}
	if gkine == "" {
		gkine = blueSpec.KineExecutable()
	}
	if r.Relet != nil {
		gp, err := portallocator.PurposesOfSlot(green)
		if err != nil {
			return fail(err)
		}
		for _, p := range gp.All() {
			lease, changed, err := r.Relet(key, p)
			if err != nil {
				return fail(fmt.Errorf("clusterprovider: leasing %s for slot %s: %w", p, green, err))
			}
			if changed {
				rep.Relet = append(rep.Relet, fmt.Sprintf("%s %d -> %d", p, l[p].Port, lease.Port))
			}
			l[p] = lease
		}
	}
	spec, err := r.Stack.SlotSpec(o, l, SlotState{Slot: green, K3s: gk3s, Kine: gkine})
	if err != nil {
		return fail(err)
	}
	front := apiserverfront.Client{Path: spec.FrontControl}
	core, err := r.Stack.Admin(ctx, key, APIServerURL(spec.AdvertisePort))
	if err != nil {
		return fail(err)
	}
	if rep.FromVersion, err = ExecutableVersion(ctx, blueSpec.K3sExecutable()); err != nil {
		return fail(err)
	}
	if rep.ToVersion, err = ExecutableVersion(ctx, spec.K3sExecutable()); err != nil {
		return fail(err)
	}
	want := fmt.Sprintf("%s:%d", spec.AdvertiseAddress, spec.AdvertisePort)
	if eps, err := AdvertisedEndpoints(ctx, core); err != nil || len(eps) != 1 || eps[0] != want {
		return fail(fmt.Errorf("clusterprovider: the kubernetes EndpointSlice is %v (%v), not only %s; refusing to swap", eps, err, want))
	}
	bound := DefaultIdleBound
	if req.IdleBound != "" {
		if bound, err = time.ParseDuration(req.IdleBound); err != nil {
			return fail(err)
		}
	}
	idleStart := time.Now()
	for {
		nodes, running, guests, err := workingPods(ctx, core)
		if err != nil {
			return fail(err)
		}
		rep.Nodes = nodes
		if err := CheckSkew(rep.FromVersion, rep.ToVersion, guests); err != nil {
			return fail(err)
		}
		if running == 0 {
			break
		}
		if time.Since(idleStart) >= bound {
			rep.IdleDeferred = true
			break
		}
		r.log().Info("cluster provider: a swap waits for the market nodes to be idle", "cluster", key, "running", running, "bound", bound)
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-time.After(idlePoll):
		}
	}
	rep.IdleSeconds = secsBetween(idleStart, time.Now())
	if am, _ := minorOf(rep.FromVersion); func() bool { tm, _ := minorOf(rep.ToVersion); return tm != am }() {
		store, ok := h.Store(key)
		if !ok {
			return fail(errors.New("clusterprovider: no store recorded for a minor bump's backup"))
		}
		dir := filepath.Join(spec.DataDir, BackupsDirName, time.Now().UTC().Format("20060102T150405Z"))
		if err := backupStore(ctx, r.Stack, store, dir); err != nil {
			return fail(err)
		}
		rep.Backup = dir
	}
	kineStep, k3sStep, err := slotSteps(spec)
	if err != nil {
		return fail(err)
	}
	abort := func(err error) RollReport {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		h.remove(sctx, ObjectName(key, StepMember(k3sStep)))
		h.remove(sctx, ObjectName(key, StepMember(kineStep)))
		return fail(err)
	}
	ko, err := h.placeGated(ctx, key, kineStep, func(ctx context.Context, m procgroup.Member) error {
		if err := waitDial(ctx, spec.KineListen); err != nil {
			return err
		}
		return waitOutput(ctx, m, kineSchemaMark, "kine@"+green+"'s schema check")
	})
	if err != nil {
		return abort(err)
	}
	so, err := h.placeGated(ctx, key, k3sStep, func(ctx context.Context, m procgroup.Member) error {
		if err := WaitSlot(ctx, spec); err != nil {
			return err
		}
		return SameCAs(blueSpec.ServerTLSDir(), spec.ServerTLSDir())
	})
	if err != nil {
		return abort(err)
	}
	rep.GreenReadyAt = time.Now().UTC()
	rep.StartToReady = secsBetween(rep.StartedAt, rep.GreenReadyAt)
	beforeNodes := nodeReadySince(ctx, core)
	beforeLeases := leaseHolders(ctx, core)

	next := cp
	next.ActiveSlot = green
	next.LastRoll = req.ID
	next.Members = nil
	for _, m := range cp.Members {
		switch m.Name {
		case "kine@" + blue:
			m = ControlPlaneMember{Name: StepMember(kineStep), Unit: ko.Spec.Unit, Object: ObjectName(key, StepMember(kineStep)), InvocationID: ko.Status.Runtime.InvocationID}
		case "k3s@" + blue:
			m = ControlPlaneMember{Name: StepMember(k3sStep), Unit: so.Spec.Unit, Object: ObjectName(key, StepMember(k3sStep)), InvocationID: so.Status.Runtime.InvocationID}
		}
		next.Members = append(next.Members, m)
	}
	if next.Slots == nil {
		next.Slots = map[string]SlotRecord{}
	}
	if _, ok := next.Slots[blue]; !ok {
		next.Slots[blue] = SlotRecord{K3s: blueSpec.K3sExecutable(), Kine: blueSpec.KineExecutable(), K3sVersion: rep.FromVersion,
			Port: blueSpec.K3sSecurePort, DataDir: blueSpec.ServerDataDir, Hostname: blueSpec.Hostname,
			K3sUnit: h.Unit(key, "k3s@"+blue), KineUnit: h.Unit(key, "kine@"+blue)}
	}
	next.Slots[green] = SlotRecord{K3s: spec.K3sExecutable(), Kine: spec.KineExecutable(), K3sVersion: rep.ToVersion,
		Port: spec.K3sSecurePort, KinePort: slotKinePort(l, green), DataDir: spec.ServerDataDir, Hostname: spec.Hostname,
		K3sUnit: so.Spec.Unit, KineUnit: ko.Spec.Unit, Invocation: so.Status.Runtime.InvocationID, StartedAt: rep.StartedAt.Format(time.RFC3339Nano)}
	if err := h.writeControlPlane(key, next); err != nil {
		return abort(err)
	}
	rep.PersistedAt = time.Now().UTC()
	fst, err := front.Set(ctx, SlotBackend(spec))
	if err != nil {
		h.writeControlPlane(key, cp)
		return abort(fmt.Errorf("clusterprovider: switching the front to slot %s: %w", green, err))
	}
	rep.FlippedAt = time.Now().UTC()
	rep.FrontBackend = fst.Backend
	r.log().Info("cluster provider: front switched", "cluster", key, "from", SlotBackend(blueSpec), "to", fst.Backend)
	if err := h.remove(ctx, ObjectName(key, "k3s@"+blue)); err != nil {
		return fail(err)
	}
	rep.OldStoppedAt = time.Now().UTC()
	rep.OldStopSecs = secsBetween(rep.FlippedAt, rep.OldStoppedAt)
	if err := h.remove(ctx, ObjectName(key, "kine@"+blue)); err != nil {
		return fail(err)
	}
	deadline := time.Now().Add(drainWait)
	for {
		st, err := front.Status(ctx)
		if err == nil {
			rep.OpenOnOld = st.Open[SlotBackend(blueSpec)]
		}
		if err != nil || rep.OpenOnOld == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(PollInterval)
	}
	if n, err := front.Drain(ctx, SlotBackend(blueSpec)); err == nil {
		rep.Drained = n
	}
	if f, err := h.frontRecord(ctx, key); err == nil {
		next.Front = f
		h.writeControlPlane(key, next)
	}
	r.watch(ctx, core, spec, &rep, beforeNodes, beforeLeases)
	return rep
}

func slotKinePort(l materialiser.Leases, slot string) int {
	sp, err := portallocator.PurposesOfSlot(slot)
	if err != nil {
		return 0
	}
	return l[sp.Kine].Port
}

func linkComponent(service v1alpha1.ClusterLinkService) (procgroup.Component, error) {
	switch service {
	case v1alpha1.ClusterLinkApiserver:
		return procgroup.ComponentApiserverLink, nil
	case v1alpha1.ClusterLinkOpenBao:
		return procgroup.ComponentOpenBaoLink, nil
	}
	return "", fmt.Errorf("clusterprovider: %q is no cluster link; the links are %v", service, clusterlink.Services)
}

func printedTicket(log string) string {
	t, _ := clusterlink.TicketIn(unitMember{path: log}.Output())
	return t
}

func (r *Roller) RollLink(ctx context.Context, key string, req RollRequest) (rep RollReport) {
	h := r.Units
	rep = RollReport{ID: req.ID, Kind: RollKindLink, Cluster: key, StartedAt: time.Now().UTC()}
	fail := func(err error) RollReport {
		rep.Error = err.Error()
		return rep
	}
	if r.Stack.Links == nil {
		return fail(errors.New("clusterprovider: this provider runs no cluster links"))
	}
	service := v1alpha1.ClusterLinkService(req.Service)
	comp, err := linkComponent(service)
	if err != nil {
		return fail(err)
	}
	unlock := h.lock(key)
	defer unlock()
	cp, err := h.readControlPlane(key)
	if err != nil {
		return fail(err)
	}
	cur := h.LinkState(key, service)
	from := slotOr(cur.Slot)
	to := otherSlot(from)
	rep.From, rep.To = from, to
	l, err := r.Leases(key)
	if err != nil {
		return fail(err)
	}
	dumbpipe := req.Dumbpipe
	if dumbpipe == "" {
		dumbpipe = cur.Dumbpipe
	}
	next := LinkRecord{Slot: to, Dumbpipe: dumbpipe}
	cfg := r.Stack
	prev := cfg.LinkSlot
	cfg.LinkSlot = func(cluster string, s v1alpha1.ClusterLinkService) LinkRecord {
		if s == service {
			return next
		}
		if prev != nil {
			return prev(cluster, s)
		}
		return LinkRecord{Slot: ActiveSlot}
	}
	cfg.Slot = func(string) SlotState { return h.SlotState(key) }
	steps, err := StepsFor(cfg)(materialiser.Observed{Cluster: v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: key}}}, l)
	if err != nil {
		return fail(err)
	}
	var st procgroup.Step
	var oldLog string
	for _, s := range steps {
		if s.Name == comp {
			st = s
		}
	}
	if st.Name == "" {
		return fail(fmt.Errorf("clusterprovider: no %s step for %s", comp, key))
	}
	oldMember := SlotMember(comp, linkStepSlot(from))
	newMember := StepMember(st)
	if o, err := h.member(ctx, key, oldMember); err == nil && o != nil && o.Spec.Output != nil {
		oldLog = o.Spec.Output.Stdout
		rep.OldListener = o.Spec.Unit
	}
	rep.TicketBefore = printedTicket(oldLog)
	rotateLog(st.Spec.Log)
	spec, err := st.Prepare(ctx, st.Spec)
	if err != nil {
		return fail(err)
	}
	st.Spec = spec
	o, err := h.placeGated(ctx, key, st, st.Ready)
	if err != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		h.remove(sctx, ObjectName(key, newMember))
		return fail(err)
	}
	rep.GreenReadyAt = time.Now().UTC()
	rep.StartToReady = secsBetween(rep.StartedAt, rep.GreenReadyAt)
	rep.NewListener = o.Spec.Unit
	rep.Ticket = printedTicket(st.Spec.Log)
	updated := cp
	updated.Members = nil
	for _, m := range cp.Members {
		if m.Name == oldMember {
			m = ControlPlaneMember{Name: newMember, Unit: o.Spec.Unit, Object: ObjectName(key, newMember), InvocationID: o.Status.Runtime.InvocationID}
		}
		updated.Members = append(updated.Members, m)
	}
	if updated.Links == nil {
		updated.Links = map[string]LinkRecord{}
	}
	updated.Links[string(service)] = next
	updated.LastRoll = req.ID
	if err := h.writeControlPlane(key, updated); err != nil {
		return fail(err)
	}
	rep.PersistedAt = time.Now().UTC()
	if err := h.remove(ctx, ObjectName(key, oldMember)); err != nil {
		return fail(err)
	}
	rep.OldStoppedAt = time.Now().UTC()
	rep.OldStopSecs = secsBetween(rep.PersistedAt, rep.OldStoppedAt)
	return rep
}

func (r *Roller) watch(ctx context.Context, core kubernetes.Interface, spec clusterstack.Spec, rep *RollReport, nodes, leases map[string]string) {
	want := fmt.Sprintf("%s:%d", spec.AdvertiseAddress, spec.AdvertisePort)
	pending := map[string]*LeaseHandover{}
	for name, holder := range leases {
		pending[name] = &LeaseHandover{Lease: name, From: holder}
	}
	end := time.Now().Add(DefaultRollWatch)
	for time.Now().Before(end) {
		if eps, err := AdvertisedEndpoints(ctx, core); err != nil || len(eps) != 1 || eps[0] != want {
			rep.EndpointsLost++
		} else {
			rep.Endpoints = eps
		}
		for name, holder := range leaseHolders(ctx, core) {
			p, ok := pending[name]
			if !ok || p.To != "" {
				continue
			}
			if holder != p.From {
				p.To = holder
				p.Seconds = secsBetween(rep.OldStoppedAt, time.Now())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	for _, p := range pending {
		rep.Leases = append(rep.Leases, *p)
	}
	after := nodeReadySince(ctx, core)
	for n, was := range nodes {
		if after[n] != was {
			rep.NodeTransitions = append(rep.NodeTransitions, n+": "+was+" -> "+after[n])
		}
	}
}
