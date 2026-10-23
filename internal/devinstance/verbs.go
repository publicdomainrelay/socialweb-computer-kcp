package devinstance

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/fsx"
)

const Marker = ".swc-dev"

const (
	EnvFile     = "env"
	EnvJSONFile = "env.json"
)

func DevRoot(stateRoot string) string { return filepath.Join(stateRoot, "dev") }

func LinkMarker(worktree, state string) error {
	p := filepath.Join(worktree, Marker)
	if cur, err := os.Readlink(p); err == nil {
		if cur == state {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return err
		}
	} else if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("devinstance: %s exists and is not a symlink; refusing to replace it", p)
	}
	return os.Symlink(state, p)
}

func UnlinkMarker(worktree, state string) bool {
	p := filepath.Join(worktree, Marker)
	if cur, err := os.Readlink(p); err != nil || cur != state {
		return false
	}
	return os.Remove(p) == nil
}

type Export struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func envWord(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func Exports(rec Record) []Export {
	l := rec.Layout
	socket := l.Socket
	if rec.Parent != nil {
		socket = rec.Parent.Socket
	}
	out := []Export{
		{"SOCIALWEB_INSTANCE", l.ID},
		{"SOCIALWEB_DEV_STATE_DIR", l.StateDir},
		{"SOCIALWEB_DEVSUP_SOCKET", socket},
		{"KUBECONFIG", rec.AdminKubeconfig},
		{"SOCIALWEB_KCP_KUBECONFIG", rec.AdminKubeconfig},
		{"SOCIALWEB_KCP_HOST", rec.KcpURL},
		{"SOCIALWEB_KCP_ROOT", l.KcpRoot},
		{"SOCIALWEB_KCP_BASE_WORKSPACE", rec.BaseWorkspace},
		{"SOCIALWEB_KINE_URL", rec.KineURL},
		{"SOCIALWEB_CLUSTERPROVIDER_UNIT_PREFIX", l.Names().HostProcessPrefix()},
		{"SOCIALWEB_CLUSTERPROVIDER_HOSTPROCESS_WORKSPACE", rec.BaseWorkspace},
	}
	for _, u := range rec.Units {
		if u.Port > 0 {
			out = append(out, Export{"SOCIALWEB_DEV_PORT_" + envWord(u.Role), strconv.Itoa(u.Port)})
		}
	}
	if rec.VM != nil {
		ws := rec.BaseWorkspace
		ws = cmp.Or(ws, "root")
		host, _, _ := strings.Cut(strings.TrimSuffix(rec.KcpURL, "/"), "/clusters/")
		out = append(out,
			Export{"COMPUTE_PROVIDER_KCP", "true"},
			Export{"KCP_SERVER", host},
			Export{"KCP_WORKSPACE", ws},
			Export{"COMPUTE_PROVIDER_KCP_HANDLE", rec.VM.Handle})
	}
	return out
}

func ShellExports(ex []Export) string {
	var b strings.Builder
	for _, e := range ex {
		b.WriteString("export " + e.Name + "='" + strings.ReplaceAll(e.Value, "'", `'\''`) + "'\n")
	}
	return b.String()
}

func List(stateRoot string) ([]Record, error) {
	entries, err := os.ReadDir(DevRoot(stateRoot))
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Record{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var rec Record
		if err := fsx.ReadJSON(filepath.Join(DevRoot(stateRoot), e.Name(), "instance.json"), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Layout.ID < out[j].Layout.ID })
	return out, nil
}

type logName struct {
	role string
	unit string
	file string
}

func logNames(l Layout) []logName {
	return []logName{
		{"pg", l.Names().Store(), filepath.Join(l.Logs, "pg.log")},
		{"kine-a", l.KineUnit(), filepath.Join(l.Logs, "kine-a.log")},
		{"front", l.FrontUnit(), filepath.Join(l.Logs, "front.log")},
		{"kcp-a", l.KcpUnit(), filepath.Join(l.Logs, "kcp-a.log")},
		{"kcp-b", l.SlotUnit("b"), filepath.Join(l.Logs, "kcp-b.log")},
		{"core", l.CoreUnit(), filepath.Join(l.Logs, "core.log")},
		{"devsup", "", filepath.Join(l.SupState, "devsup.log")},
	}
}

func LogNames(l Layout) []string {
	var out []string
	for _, n := range logNames(l) {
		out = append(out, n.role)
	}
	return out
}

func LogFile(l Layout, name string) (string, error) {
	for _, n := range logNames(l) {
		if name == n.role || (n.unit != "" && name == n.unit) {
			return n.file, nil
		}
	}
	return "", fmt.Errorf("devinstance: no unit %q in instance %s: want one of %s or its unit name", name, l.ID, strings.Join(LogNames(l), ", "))
}
