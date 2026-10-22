//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vmruntime"
)

const (
	EnvAccount    = "SOCIALWEB_IT_ACCOUNT"
	EnvPublish    = "SOCIALWEB_IT_PUBLISH"
	EnvSkipBuild  = "SOCIALWEB_IT_SKIP_BUILD"
	EnvGuestImage = "SOCIALWEB_IT_GUEST_IMAGE"
	EnvArtifacts  = "SOCIALWEB_IT_ARTIFACTS"
	EnvRunRoot    = "SOCIALWEB_IT_RUN_ROOT"
	EnvLiveSlot   = "SOCIALWEB_IT_LIVE_SLOT"
	EnvDumbpipe   = "SOCIALWEB_IT_DUMBPIPE"

	liveSlotWait = 30 * time.Minute
	liveSlotBeat = 10 * time.Minute

	defaultRunRoot    = "/var/tmp/swcit"
	defaultAccount    = "test-0001"
	defaultGuestImage = "file:///var/lib/socialweb-computer/guest-image"

	defaultDumbpipe = "/usr/local/bin/dumbpipe"

	buildTimeout   = 15 * time.Minute
	upTimeout      = 4 * time.Minute
	downTimeout    = 6 * time.Minute
	commandTimeout = 30 * time.Second
	healthEvery    = 3 * time.Second
	healthTimeout  = 20 * time.Second
	maxSocketPath  = 107

	verifierFailureGrace = 3 * time.Minute
)

type Harness struct {
	Repo      string
	Publish   string
	Bin       string
	Account   string
	UID       int
	Home      string
	Sudo      bool
	Image     string
	RunID     string
	Root      string
	Tmp       string
	Artifacts string
}

var h *Harness

func Main(m *testing.M) int {
	var err error
	if h, err = setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		return 1
	}
	code := m.Run()
	if !stopSharedNetworks() && code == 0 {
		code = 1
	}
	if err := h.removeRoot(); err != nil {
		fmt.Fprintf(os.Stderr, "integration: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	fmt.Fprintf(os.Stderr, "integration: logs of this run under %s\n", h.Artifacts)
	return code
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func dumbpipePath(t testing.TB) string {
	t.Helper()
	if v := os.Getenv(EnvDumbpipe); v != "" {
		return v
	}
	if st, err := os.Stat(defaultDumbpipe); err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0 {
		return defaultDumbpipe
	}
	if path, err := exec.LookPath("dumbpipe"); err == nil {
		return path
	}
	t.Fatalf("no dumbpipe executable: set %s, install %s, or put dumbpipe on PATH; the ssh gateway reaches guests over iroh only", EnvDumbpipe, defaultDumbpipe)
	return ""
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func setup() (*Harness, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil, errors.New("no caller information to find the repository")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	hs := &Harness{Repo: repo, Account: envOr(EnvAccount, defaultAccount), Image: envOr(EnvGuestImage, defaultGuestImage)}
	acct, err := user.Lookup(hs.Account)
	if err != nil {
		return nil, fmt.Errorf("the VM account %s (%s): %w", hs.Account, EnvAccount, err)
	}
	if hs.UID, err = strconv.Atoi(acct.Uid); err != nil {
		return nil, err
	}
	hs.Home = acct.HomeDir
	cur, err := user.Current()
	if err != nil {
		return nil, err
	}
	hs.Sudo = cur.Uid != acct.Uid
	hs.RunID = randHex(2)
	hs.Artifacts = envOr(EnvArtifacts, filepath.Join(cur.HomeDir, ".tmp", "swc-it",
		strings.ToLower(time.Now().UTC().Format("20060102t150405"))+"-"+hs.RunID))
	if err := os.MkdirAll(hs.Artifacts, 0o755); err != nil {
		return nil, err
	}
	hs.Publish = envOr(EnvPublish, "/var/tmp/swc-it-"+filepath.Base(repo))
	if err := checkImage(hs.Image); err != nil {
		return nil, err
	}
	if hs.Sudo {
		if out, err := exec.Command("sudo", "-n", "-u", hs.Account, "true").CombinedOutput(); err != nil {
			return nil, fmt.Errorf("sudo -n -u %s refused (run as %s or allow it): %v %s", hs.Account, hs.Account, err, out)
		}
	}
	if os.Getenv(EnvSkipBuild) == "" {
		if err := hs.build(); err != nil {
			return nil, err
		}
	}
	hs.Bin = filepath.Join(hs.Publish, "bin", "socialweb-computer")
	if _, err := os.Stat(hs.Bin); err != nil {
		return nil, fmt.Errorf("no published binary (%s unset or a failed build): %w", EnvSkipBuild, err)
	}
	runs := envOr(EnvRunRoot, defaultRunRoot)
	if err := os.MkdirAll(runs, 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(runs); err != nil || fi.Mode().Perm() != 0o777 || fi.Mode()&os.ModeSticky == 0 {
		if err := os.Chmod(runs, 0o777|os.ModeSticky); err != nil {
			return nil, fmt.Errorf("%s (%s) must be mode 1777 so the VM account can make its run dir there: %w", runs, EnvRunRoot, err)
		}
	}
	hs.Root = filepath.Join(runs, hs.RunID)
	if _, err := os.Stat(hs.Root); err == nil {
		return nil, fmt.Errorf("the run dir %s exists; another run holds it", hs.Root)
	}
	hs.Tmp = filepath.Join(hs.Root, "tmp")
	if r := hs.As(context.Background(), "mkdir", "-p", "-m", "0755", hs.Root, hs.Tmp); r.Err != nil {
		return nil, fmt.Errorf("creating the run root as %s: %v", hs.Account, r.Err)
	}
	return hs, nil
}

func checkImage(src string) error {
	dir, ok := strings.CutPrefix(src, "file://")
	if !ok {
		return nil
	}
	for _, f := range []string{"vmlinux", "rootfs.ext4", "image.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return fmt.Errorf("the guest image source %s (%s) has no %s: %w", src, EnvGuestImage, f, err)
		}
	}
	return nil
}

func (hs *Harness) build() error {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	log, err := os.Create(filepath.Join(hs.Artifacts, "build.log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.CommandContext(ctx, filepath.Join(hs.Repo, "scripts", "build-socialweb-computer.sh"), "--dev", "--publish", hs.Publish)
	cmd.Dir, cmd.Stdout, cmd.Stderr = hs.Repo, log, log
	started := time.Now()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("scripts/build-socialweb-computer.sh --dev --publish %s: %v (log %s)", hs.Publish, err, log.Name())
	}
	fmt.Fprintf(os.Stderr, "integration: built and published %s in %.1fs\n", hs.Publish, time.Since(started).Seconds())
	return nil
}

func (hs *Harness) liveSlotTool() string {
	if v := os.Getenv(EnvLiveSlot); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".tmp", "swc-lead", "live-slot.ts")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func (hs *Harness) liveSlot(t testing.TB, name string) {
	t.Helper()
	tool := hs.liveSlotTool()
	if tool == "" || tool == "none" {
		return
	}
	slot := func(ctx context.Context, verb string) error {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		cmd := exec.CommandContext(ctx, "deno", "run", "-A", tool, verb, name)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("live-slot %s %s: %v: %s", verb, name, err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), liveSlotWait)
	defer cancel()
	if err := slot(ctx, "acquire"); err != nil {
		t.Fatalf("no live slot for a market network within %s (%s): %v", liveSlotWait, EnvLiveSlot, err)
	}
	t.Logf("live slot %s held after %.1fs of waiting", name, time.Since(started).Seconds())
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(liveSlotBeat)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				slot(context.Background(), "beat")
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		if err := slot(context.Background(), "release"); err != nil {
			t.Errorf("%v", err)
		}
	})
}

func (hs *Harness) removeRoot() error {
	if hs.Root == "" {
		return nil
	}
	if r := hs.As(context.Background(), "rm", "-rf", hs.Root); r.Err != nil {
		return fmt.Errorf("removing the run root %s: %v", hs.Root, r.Err)
	}
	return nil
}

func (hs *Harness) env() []string {
	return []string{"HOME=" + hs.Home, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", hs.UID),
		fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", hs.UID), "PATH=/usr/local/bin:/usr/bin:/bin",
		"TMPDIR=" + hs.Tmp, "LANG=C.UTF-8", "SOCIALWEB_CLUSTERPROVIDER_SCALE_HOLD=" + scaleHold.String()}
}

func (hs *Harness) Command(ctx context.Context, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	dir := hs.Root
	if _, err := os.Stat(dir); dir == "" || err != nil {
		dir = "/"
	}
	if hs.Sudo {
		sargs := append([]string{"-n", "-u", hs.Account, "env", "--chdir=" + dir}, hs.env()...)
		cmd = exec.CommandContext(ctx, "sudo", append(sargs, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, args[0], args[1:]...)
		cmd.Env, cmd.Dir = hs.env(), dir
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 20 * time.Second
	return cmd
}

type Result struct {
	Label   string
	Code    int
	Stdout  string
	Stderr  string
	Elapsed time.Duration
	Err     error
}

func (r Result) Tail(n int) string {
	lines := strings.Split(strings.TrimRight(r.Stderr, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (hs *Harness) As(ctx context.Context, args ...string) Result {
	return hs.AsInput(ctx, nil, args...)
}

func (hs *Harness) AsInput(ctx context.Context, stdin []byte, args ...string) Result {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	var out, errb bytes.Buffer
	cmd := hs.Command(ctx, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	started := time.Now()
	err := cmd.Run()
	r := Result{Label: args[0], Code: cmd.ProcessState.ExitCode(), Stdout: out.String(), Stderr: errb.String(), Elapsed: time.Since(started)}
	if err != nil {
		r.Err = fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return r
}

func (hs *Harness) WriteFile(path string, data []byte, mode os.FileMode) error {
	r := hs.AsInput(context.Background(), data, "sh", "-c", `umask 077 && cat > "$1" && chmod "$2" "$1"`, "sh", path, fmt.Sprintf("%04o", mode))
	return r.Err
}

type NetworkOptions struct {
	Bidders     int
	WithGateway bool
	UpTimeout   time.Duration

	Dumbpipe string

	GatewayOptions map[string]string
}

type Network struct {
	h         *Harness
	t         testing.TB
	Instance  string
	Dir       string
	StateRoot string
	StateDir  string
	Handles   []string
	Logs      string
	Started   time.Time
	Up        UpReport

	mu         sync.Mutex
	runs       []string
	seenDevsup bool
	probeSince time.Time
}

type UpReport struct {
	Instance        string                  `json:"instance"`
	StateDir        string                  `json:"stateDir"`
	AdminKubeconfig string                  `json:"adminKubeconfig"`
	Seconds         float64                 `json:"seconds"`
	Network         *v1alpha1.MarketNetwork `json:"network"`
}

type StatusReport struct {
	Found  bool `json:"found"`
	Devsup struct {
		Reachable bool   `json:"reachable"`
		Error     string `json:"error"`
	} `json:"devsup"`
	Network      *v1alpha1.MarketNetwork `json:"network"`
	NetworkError string                  `json:"networkError"`
}

type DownReport struct {
	Instance       string   `json:"instance"`
	DevsupStopped  bool     `json:"devsupStopped"`
	LeftoverKilled []int    `json:"leftoverKilled"`
	Remaining      []int    `json:"remaining"`
	VMsStopped     []string `json:"vmsStopped"`
	VMsLeft        []string `json:"vmsLeft"`
	RemovedState   bool     `json:"removedState"`
	Seconds        float64  `json:"seconds"`
}

var testName = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

func StartNetwork(t testing.TB, opts NetworkOptions) *Network {
	t.Helper()
	if opts.UpTimeout == 0 {
		opts.UpTimeout = upTimeout
	}
	inst := "i" + randHex(3)
	n := &Network{h: h, t: t, Instance: inst, Dir: filepath.Join(h.Root, "f-"+inst), StateRoot: h.Root, Started: time.Now()}
	n.StateDir = filepath.Join(n.StateRoot, "dev", inst)
	if long := filepath.Join(n.StateDir, "apps", "dev", inst+"-gateway", "clusterprovider", "fronts", randHex(8)+".sock"); len(long) > maxSocketPath {
		t.Fatalf("the state dir %s is too long: a tenant cluster's front socket would be %d bytes, over the %d-byte sun_path limit (%s)",
			n.StateDir, len(long), maxSocketPath, EnvRunRoot)
	}
	n.Logs = filepath.Join(h.Artifacts, testName.ReplaceAllString(t.Name(), "_")+"-"+inst)
	if err := os.MkdirAll(n.Logs, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= opts.Bidders; i++ {
		n.Handles = append(n.Handles, fmt.Sprintf("bidder-%d.%s.bob.test", i, inst))
	}
	t.Logf("market network %s: %d bidder(s), gateway %t, state %s, logs %s", inst, opts.Bidders, opts.WithGateway, n.StateDir, n.Logs)
	if r := h.As(context.Background(), "mkdir", "-p", "-m", "0755", n.Dir); r.Err != nil {
		t.Fatal(r.Err)
	}
	n.h.liveSlot(t, filepath.Base(n.h.Repo)+"-"+inst)
	t.Cleanup(n.cleanup)
	values := map[string]any{"bidder": map[string]any{"kcp": map[string]any{"handle": "{app}." + inst + ".bob.test"}}}
	if len(opts.GatewayOptions) > 0 {
		var gwOptions []map[string]string
		for _, k := range sortedKeys(opts.GatewayOptions) {
			gwOptions = append(gwOptions, map[string]string{"name": k, "value": opts.GatewayOptions[k]})
		}
		values["gateway"] = map[string]any{"options": gwOptions}
	}
	valuesJSON, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	valuesFile := filepath.Join(n.Dir, "app-values.json")
	if err := h.WriteFile(valuesFile, valuesJSON, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"dev", "up", "--instance", inst, "--profile", "market-network", "--bidders", strconv.Itoa(opts.Bidders),
		"--guest-image-source", h.Image, "--vm-host-backend", "systemd-user", "--app-values", valuesFile,
		"--ready-timeout", "90s", "--network-timeout", opts.UpTimeout.String(), "--state-dir", n.StateRoot}
	if opts.WithGateway {
		args = append(args, "--with-gateway")
	}
	if opts.Dumbpipe != "" {
		args = append(args, "--dumbpipe-path", opts.Dumbpipe)
	}
	r := n.Run("dev-up", opts.UpTimeout+30*time.Second, args...)
	if r.Err != nil {
		t.Fatalf("dev up: %v\n%s", r.Err, r.Tail(15))
	}
	if err := json.Unmarshal([]byte(r.Stdout), &n.Up); err != nil {
		t.Fatalf("dev up printed no report: %v", err)
	}
	if n.Up.Network == nil || n.Up.Network.Status.Phase != v1alpha1.ApplicationPhaseReady {
		t.Fatalf("dev up returned without a Ready MarketNetwork")
	}
	t.Logf("market network %s Ready in %.1fs (dev up %.1fs)", inst, n.Up.Seconds, r.Elapsed.Seconds())
	return n
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (n *Network) swc(args ...string) []string {
	return append([]string{n.h.Bin}, args...)
}

func (n *Network) Run(label string, timeout time.Duration, args ...string) Result {
	return n.Start(label, timeout, args...).Wait()
}

func (n *Network) RunUnwatched(label string, timeout time.Duration, args ...string) Result {
	return n.start(label, timeout, false, args...).Wait()
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type Proc struct {
	n       *Network
	label   string
	base    string
	timeout time.Duration
	ctx     context.Context
	cancel  context.CancelFunc
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	out     lockedBuffer
	errb    lockedBuffer
	files   []*os.File
	started time.Time
	done    chan struct{}
	werr    error
	reason  error
	result  *Result
}

func (n *Network) Start(label string, timeout time.Duration, args ...string) *Proc {
	return n.start(label, timeout, true, args...)
}

func (n *Network) start(label string, timeout time.Duration, watch bool, args ...string) *Proc {
	n.mu.Lock()
	n.runs = append(n.runs, label)
	idx := len(n.runs)
	n.mu.Unlock()
	p := &Proc{n: n, label: label, timeout: timeout, base: filepath.Join(n.Logs, fmt.Sprintf("%02d-%s", idx, label)), done: make(chan struct{})}
	p.ctx, p.cancel = context.WithTimeout(context.Background(), timeout)
	ofile, _ := os.Create(p.base + ".stdout")
	efile, _ := os.Create(p.base + ".stderr")
	p.files = []*os.File{ofile, efile}
	p.cmd = n.h.Command(p.ctx, n.swc(args...)...)
	p.cmd.Stdout, p.cmd.Stderr = io.MultiWriter(&p.out, ofile), io.MultiWriter(&p.errb, efile)
	p.stdin, p.werr = p.cmd.StdinPipe()
	p.started = time.Now()
	if p.werr == nil {
		p.werr = p.cmd.Start()
	}
	if p.werr != nil {
		close(p.done)
		return p
	}
	go func() {
		p.werr = p.cmd.Wait()
		close(p.done)
	}()
	if watch {
		n.t.Cleanup(func() {
			select {
			case <-p.done:
			default:
				p.Kill(errors.New("the test ended"))
				p.Wait()
			}
		})
	}
	if watch {
		go p.watch()
	}
	return p
}

func (p *Proc) watch() {
	tick := time.NewTicker(healthEvery)
	defer tick.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-tick.C:
			if err := p.n.health(); err != nil {
				p.n.mu.Lock()
				if p.reason == nil {
					p.reason = err
				}
				p.n.mu.Unlock()
				p.n.t.Logf("%s: stopping at once: %v", p.label, err)
				p.cancel()
				return
			}
		}
	}
}

func (p *Proc) Stdout() string { return p.out.String() }

func (p *Proc) Input(s string) error {
	_, err := io.WriteString(p.stdin, s)
	return err
}

func (p *Proc) Kill(reason error) {
	p.n.mu.Lock()
	if p.reason == nil {
		p.reason = reason
	}
	p.n.mu.Unlock()
	p.cancel()
}

func (p *Proc) WaitOutput(marker string) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if strings.Contains(p.out.String(), marker) {
			return nil
		}
		select {
		case <-p.done:
			if strings.Contains(p.out.String(), marker) {
				return nil
			}
			r := p.Wait()
			if r.Err != nil {
				return r.Err
			}
			return fmt.Errorf("%s exited without printing %q", p.label, marker)
		case <-tick.C:
		}
	}
}

func (p *Proc) Wait() Result {
	<-p.done
	if p.result != nil {
		return *p.result
	}
	p.stdin.Close()
	for _, f := range p.files {
		f.Close()
	}
	res := Result{Label: p.label, Elapsed: time.Since(p.started), Stdout: p.out.String(), Stderr: p.errb.String(), Code: -1}
	if p.cmd.ProcessState != nil {
		res.Code = p.cmd.ProcessState.ExitCode()
	}
	p.n.mu.Lock()
	reason := p.reason
	p.n.mu.Unlock()
	switch {
	case reason != nil:
		res.Err = fmt.Errorf("%s ended early after %.1fs: %w", p.label, res.Elapsed.Seconds(), reason)
	case p.ctx.Err() == context.DeadlineExceeded:
		res.Err = fmt.Errorf("%s did not finish within %s (an undetected failure: see %s.stderr)", p.label, p.timeout, p.base)
	case p.werr != nil:
		res.Err = fmt.Errorf("%s exited %d after %.1fs: %v", p.label, res.Code, res.Elapsed.Seconds(), lastError(res.Stderr))
	}
	p.cancel()
	p.n.t.Logf("%s: exit %d in %.1fs", p.label, res.Code, res.Elapsed.Seconds())
	p.result = &res
	return res
}

func lastError(stderr string) string {
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) == nil {
			if rec["level"] == "ERROR" || rec["level"] == "WARN" {
				return l
			}
			continue
		}
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return "no error line"
}

func (n *Network) Status() (StatusReport, error) {
	var st StatusReport
	ctx, cancel := context.WithTimeout(context.Background(), healthTimeout)
	defer cancel()
	r := n.h.As(ctx, n.swc("dev", "status", "--instance", n.Instance, "--state-dir", n.StateRoot)...)
	if r.Stdout == "" {
		return st, fmt.Errorf("dev status: %v", r.Err)
	}
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
		return st, fmt.Errorf("dev status: %w", err)
	}
	return st, nil
}

func (n *Network) health() error {
	st, err := n.Status()
	if err != nil {
		return nil
	}
	n.mu.Lock()
	seen := n.seenDevsup
	if st.Devsup.Reachable {
		n.seenDevsup = true
	}
	n.mu.Unlock()
	if !st.Devsup.Reachable && seen {
		return fmt.Errorf("the network's devsup is gone: %s", st.Devsup.Error)
	}
	if st.Network == nil {
		return nil
	}
	s := st.Network.Status
	if s.Phase == v1alpha1.ApplicationPhaseFailed {
		return fmt.Errorf("the MarketNetwork is Failed: %s", s.Message)
	}
	probe, message := false, ""
	for _, c := range s.Conditions {
		if c.Type == "VerifierPassed" && c.Status == metav1.ConditionFalse && c.Reason == "Probe" {
			probe, message = true, c.Message
		}
	}
	n.mu.Lock()
	if !probe {
		n.probeSince = time.Time{}
	} else if n.probeSince.IsZero() {
		n.probeSince = time.Now()
	}
	failedFor := time.Since(n.probeSince)
	n.mu.Unlock()
	if probe && failedFor >= verifierFailureGrace {
		return fmt.Errorf("the network verifier has failed for %s: %s", failedFor.Round(time.Second), message)
	}
	for _, a := range s.Apps {
		if a.Phase == v1alpha1.ApplicationPhaseFailed {
			return fmt.Errorf("child application %s (%s) is Failed: %s", a.Name, a.Kind, a.Message)
		}
		for _, c := range a.Components {
			if c.Kind == "ComputeVM" && c.Phase == string(v1alpha1.ComputeVMPhaseFailed) {
				return fmt.Errorf("ComputeVM %s of %s is Failed", c.Name, a.Name)
			}
		}
	}
	return nil
}

func (n *Network) App(name string) (v1alpha1.NetworkAppStatus, bool) {
	st, err := n.Status()
	if err != nil || st.Network == nil {
		return v1alpha1.NetworkAppStatus{}, false
	}
	for _, a := range st.Network.Status.Apps {
		if a.Name == name {
			return a, true
		}
	}
	return v1alpha1.NetworkAppStatus{}, false
}

func (n *Network) ComputeVMs() ([]string, error) {
	st, err := n.Status()
	if err != nil {
		return nil, err
	}
	if st.Network == nil {
		return nil, fmt.Errorf("dev status shows no MarketNetwork: %s", st.NetworkError)
	}
	var out []string
	for _, a := range st.Network.Status.Apps {
		for _, c := range a.Components {
			if c.Kind == "ComputeVM" {
				out = append(out, a.Name+"/"+c.Name+"="+c.Phase)
			}
		}
	}
	return out, nil
}

func (n *Network) cleanup() {
	t := n.t
	if t.Failed() {
		n.diagnose()
	}
	r := n.RunUnwatched("dev-down", downTimeout, "dev", "down", "--instance", n.Instance, "--state-dir", n.StateRoot)
	var d DownReport
	if err := json.Unmarshal([]byte(r.Stdout), &d); err != nil {
		t.Errorf("dev down printed no report (%v): %v\n%s", err, r.Err, r.Tail(10))
	} else {
		t.Logf("dev down %s in %.1fs: VMs stopped %d, leftover killed %v", n.Instance, d.Seconds, len(d.VMsStopped), d.LeftoverKilled)
		if len(d.Remaining) > 0 {
			t.Errorf("dev down left processes %v", d.Remaining)
		}
		if len(d.VMsLeft) > 0 {
			t.Errorf("dev down left VMs: %s", strings.Join(d.VMsLeft, "; "))
		}
		if !d.RemovedState {
			t.Errorf("dev down kept the state dir %s", n.StateDir)
		}
	}
	for _, left := range n.Leftovers() {
		t.Errorf("left behind after dev down: %s", left)
	}
	if r := n.h.As(context.Background(), "rm", "-rf", n.Dir); r.Err != nil {
		t.Errorf("removing %s: %v", n.Dir, r.Err)
	}
}

func (n *Network) Leftovers() []string {
	var out []string
	ctx := context.Background()
	if r := n.h.As(ctx, "test", "-e", n.StateDir); r.Code == 0 {
		out = append(out, "the state dir "+n.StateDir)
	}
	if r := n.h.As(ctx, "find", n.StateDir, "-maxdepth", "12", "-path", "*/handles/*/vms/*", "-prune"); strings.TrimSpace(r.Stdout) != "" {
		out = append(out, "VM work dirs: "+strings.Join(strings.Fields(r.Stdout), " "))
	}
	pids, err := n.processesMentioning(n.StateDir)
	if err != nil {
		out = append(out, "process scan failed: "+err.Error())
	}
	for _, p := range pids {
		out = append(out, "process "+p)
	}
	for _, hd := range n.Handles {
		id := vmruntime.HandleID(hd)
		r := n.h.As(ctx, "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "--no-pager",
			vmruntime.UnitPrefix(hd)+"*", vmruntime.SliceName(hd))
		if r.Err != nil {
			out = append(out, "listing the units of "+hd+": "+r.Err.Error())
		} else if s := strings.TrimSpace(r.Stdout); s != "" {
			out = append(out, "units of "+hd+": "+s)
		}
		cg := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/socialweb.slice/socialweb-b.slice/%s", n.h.UID, n.h.UID, vmruntime.SliceName(hd))
		if _, err := os.Stat(cg); err == nil {
			out = append(out, "cgroup "+cg+" of handle id "+id)
		}
	}
	return out
}

func (n *Network) guestsLeft() []string {
	var out []string
	vms, err := n.ComputeVMs()
	if err != nil {
		return []string{err.Error()}
	}
	for _, v := range vms {
		out = append(out, "ComputeVM "+v)
	}
	if r := n.h.As(context.Background(), "find", n.StateDir, "-maxdepth", "12", "-path", "*/handles/*/vms/*", "-prune"); strings.TrimSpace(r.Stdout) != "" {
		out = append(out, "VM work dirs "+strings.Join(strings.Fields(r.Stdout), " "))
	}
	for _, hd := range n.Handles {
		r := n.h.As(context.Background(), "systemctl", "--user", "list-units", "--all", "--plain", "--no-legend", "--no-pager", vmruntime.UnitPrefix(hd)+"*")
		if s := strings.TrimSpace(r.Stdout); s != "" {
			out = append(out, "VM units "+s)
		}
	}
	return out
}

func (n *Network) WaitNoGuests(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		left := n.guestsLeft()
		if len(left) == 0 {
			return nil
		}
		if err := n.health(); err != nil {
			return fmt.Errorf("waiting for the guests to go: %w (left: %s)", err, strings.Join(left, "; "))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("guests left %s after the release: %s", timeout, strings.Join(left, "; "))
		}
		time.Sleep(2 * time.Second)
	}
}

func (n *Network) FilesNamed(dir string, names ...string) ([]string, error) {
	args := []string{"find", dir, "-type", "f", "("}
	for i, nm := range names {
		if i > 0 {
			args = append(args, "-o")
		}
		args = append(args, "-name", nm)
	}
	r := n.h.As(context.Background(), append(args, ")")...)
	return strings.Fields(r.Stdout), r.Err
}

func (n *Network) patternFile(values ...string) (string, func(), error) {
	p := filepath.Join(n.h.Root, "pattern-"+randHex(6))
	if err := n.h.WriteFile(p, []byte(strings.Join(values, "\n")+"\n"), 0o600); err != nil {
		return "", nil, err
	}
	return p, func() { n.h.As(context.Background(), "rm", "-f", p) }, nil
}

func (n *Network) processesMentioning(values ...string) ([]string, error) {
	p, rm, err := n.patternFile(values...)
	if err != nil {
		return nil, err
	}
	defer rm()
	r := n.h.As(context.Background(), "sh", "-c",
		`for f in /proc/[0-9]*/cmdline /proc/[0-9]*/environ; do grep -q -a -F -f "$1" "$f" 2>/dev/null && echo "$f $(tr '\0' ' ' < "${f%/*}/cmdline" 2>/dev/null | cut -c1-200)"; done; true`,
		"sh", p)
	if r.Err != nil {
		return nil, r.Err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if l != "" && !strings.Contains(l, p) {
			out = append(out, l)
		}
	}
	return out, nil
}

func (n *Network) FilesContaining(dirs []string, values []string, excludeDirs ...string) ([]string, error) {
	p, rm, err := n.patternFile(values...)
	if err != nil {
		return nil, err
	}
	defer rm()
	args := []string{"grep", "-r", "-l", "-a", "-D", "skip", "-F", "-f", p}
	for _, d := range excludeDirs {
		args = append(args, "--exclude-dir="+d)
	}
	r := n.h.As(context.Background(), append(args, dirs...)...)
	if r.Code == 2 {
		for _, l := range strings.Split(strings.TrimSpace(r.Stderr), "\n") {
			if l != "" && !strings.Contains(l, "No such file or directory") {
				return nil, fmt.Errorf("grep under %v: %s", dirs, l)
			}
		}
	} else if r.Code != 0 && r.Code != 1 {
		return nil, r.Err
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func (n *Network) coreOf(dir string) (int, error) {
	r := n.h.As(context.Background(), "pgrep", "-f", "-u", fmt.Sprint(n.h.UID), "--", "[i]nternal dev-core --instance-dir "+dir+" ")
	fields := strings.Fields(r.Stdout)
	if len(fields) != 1 {
		return 0, fmt.Errorf("want one dev-core of %s, found %q", dir, strings.TrimSpace(r.Stdout))
	}
	var pid int
	_, err := fmt.Sscan(fields[0], &pid)
	return pid, err
}

func (n *Network) JournalContains(values []string, since time.Time) (int, error) {
	p, rm, err := n.patternFile(values...)
	if err != nil {
		return 0, err
	}
	defer rm()
	r := n.h.As(context.Background(), "sh", "-c",
		`journalctl --user --no-pager -o cat --since "@$2" | grep -c -a -F -f "$1"; true`, "sh", p, strconv.FormatInt(since.Unix(), 10))
	if r.Err != nil {
		return 0, r.Err
	}
	c, err := strconv.Atoi(strings.TrimSpace(r.Stdout))
	if err != nil {
		return 0, fmt.Errorf("journalctl: %q %s", r.Stdout, r.Stderr)
	}
	return c, nil
}

func (n *Network) LogsContaining(values []string) []string {
	var out []string
	filepath.WalkDir(n.Logs, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		for _, v := range values {
			if bytes.Contains(b, []byte(v)) {
				out = append(out, p)
				break
			}
		}
		return nil
	})
	return out
}

func (n *Network) diagnose() {
	t := n.t
	st, err := n.Status()
	if err != nil {
		t.Logf("diagnose: %v", err)
	} else {
		b, _ := json.MarshalIndent(st, "", "  ")
		os.WriteFile(filepath.Join(n.Logs, "status-on-failure.json"), b, 0o644)
		if st.Network != nil {
			s := st.Network.Status
			t.Logf("MarketNetwork: phase %s: %s", s.Phase, s.Message)
			for _, c := range s.Conditions {
				t.Logf("  condition %s=%s %s: %s", c.Type, c.Status, c.Reason, c.Message)
			}
			for _, c := range s.Components {
				if !c.Ready {
					t.Logf("  component %s/%s %s phase %s not ready", c.Workspace, c.Kind, c.Name, c.Phase)
				}
			}
			for _, a := range s.Apps {
				t.Logf("  app %s (%s): phase %s: %s", a.Name, a.Kind, a.Phase, a.Message)
				for _, c := range a.Components {
					t.Logf("    component %s/%s %s phase %s ready %t", c.Workspace, c.Kind, c.Name, c.Phase, c.Ready)
				}
			}
		} else {
			t.Logf("no MarketNetwork: %s", st.NetworkError)
		}
	}
	n.collectPodLogs()
}

func (n *Network) collectPodLogs() {
	t := n.t
	r := n.h.As(context.Background(), "find", n.StateDir, "(", "-name", "tmp", "-o", "-name", "kcp", "-o", "-name", "handles",
		"-o", "-name", "components", "-o", "-name", "clusterprovider", ")", "-prune",
		"-o", "-type", "f", "-name", "core.log", "-print")
	files := strings.Fields(r.Stdout)
	sort.Strings(files)
	dir := filepath.Join(n.Logs, "pods")
	os.MkdirAll(dir, 0o755)
	consoles := n.h.As(context.Background(), "find", n.StateDir, "-path", "*/handles/*/vms/*", "(", "-name", vmruntime.ConsoleFile, "-o", "-name", vmruntime.VMMLogFile, ")")
	for _, f := range strings.Fields(consoles.Stdout) {
		c := n.h.As(context.Background(), "tail", "-c", "200000", f)
		rel, _ := filepath.Rel(n.StateDir, f)
		os.WriteFile(filepath.Join(dir, strings.ReplaceAll(rel, "/", "_")), []byte(c.Stdout), 0o644)
		if strings.HasSuffix(f, vmruntime.ConsoleFile) {
			lines := strings.Split(strings.TrimRight(c.Stdout, "\n"), "\n")
			if len(lines) > 6 {
				lines = lines[len(lines)-6:]
			}
			for _, l := range lines {
				if len(l) > 300 {
					l = l[:300]
				}
				t.Logf("  guest %s: %s", filepath.Base(filepath.Dir(f)), l)
			}
		}
	}
	for _, f := range files {
		c := n.h.As(context.Background(), "tail", "-c", "2000000", f)
		rel, _ := filepath.Rel(n.StateDir, f)
		os.WriteFile(filepath.Join(dir, strings.ReplaceAll(rel, "/", "_")), []byte(c.Stdout), 0o644)
		if strings.HasSuffix(f, "core.log") {
			lines := strings.Split(strings.TrimRight(c.Stdout, "\n"), "\n")
			var bad []string
			for _, l := range lines {
				if strings.Contains(l, `"level":"ERROR"`) || strings.Contains(l, `"level":"error"`) || strings.Contains(l, "error:") {
					bad = append(bad, l)
				}
			}
			if len(bad) > 3 {
				bad = bad[len(bad)-3:]
			}
			for _, l := range bad {
				if len(l) > 400 {
					l = l[:400]
				}
				t.Logf("  %s: %s", rel, l)
			}
		}
	}
	t.Logf("pod logs copied to %s", dir)
}
