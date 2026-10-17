package request

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
	denov1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/internal/denokcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpclient"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcpserver"
)

const (
	RequestsDir      = "requests"
	InvocationFile   = "invocation.json"
	SummaryFile      = "summary.json"
	SSHDir           = "ssh"
	ModeNetwork      = "network"
	ModeStandalone   = "standalone"
	RootName         = "app"
	FieldManager     = "socialweb-computer-run-request"
	ContractTTL      = "8d"
	SecretsPath      = "contracts/" + appctl.RequesterContract + "/secrets"
	Poll             = time.Second
	irohScheme       = "iroh://"
	maxNameLength    = 30
	networkConflicts = 10
)

var nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func CheckName(name string) error {
	if !nameRE.MatchString(name) || len(name) > maxNameLength {
		return fmt.Errorf("request: name %q is not a DNS label of at most %d characters", name, maxNameLength)
	}
	return nil
}

func Root(stateRoot string) string { return filepath.Join(stateRoot, RequestsDir) }

func Dir(stateRoot, name string) string { return filepath.Join(Root(stateRoot), name) }

type Invocation struct {
	Name      string    `json:"name"`
	Mode      string    `json:"mode"`
	Network   string    `json:"network,omitempty"`
	Instance  string    `json:"instance"`
	StateRoot string    `json:"stateRoot"`
	CreatedAt time.Time `json:"createdAt"`
}

type ContractSummary struct {
	Phase         string   `json:"phase,omitempty"`
	WinnerDid     string   `json:"winnerDid,omitempty"`
	Bids          int      `json:"bids,omitempty"`
	ReceiptUri    string   `json:"receiptUri,omitempty"`
	VMAddress     string   `json:"vmAddress,omitempty"`
	HostKeys      []string `json:"hostKeys,omitempty"`
	ReleaseOK     *bool    `json:"releaseOk,omitempty"`
	ReleaseStatus int      `json:"releaseStatus,omitempty"`
	Termination   string   `json:"termination,omitempty"`
}

type Summary struct {
	Name             string           `json:"name"`
	Mode             string           `json:"mode"`
	Network          string           `json:"network,omitempty"`
	Instance         string           `json:"instance"`
	KcpURL           string           `json:"kcpURL,omitempty"`
	KcpColdSeconds   float64          `json:"kcpColdSeconds,omitempty"`
	Did              string           `json:"did,omitempty"`
	Phase            string           `json:"phase,omitempty"`
	Message          string           `json:"message,omitempty"`
	SecretsUrl       string           `json:"secretsUrl,omitempty"`
	SecretsFetchedAt string           `json:"secretsFetchedAt,omitempty"`
	Contract         *ContractSummary `json:"contract,omitempty"`
	KcpRunning       bool             `json:"kcpRunning"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

func writeJSON(path string, v any) error {
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

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func WriteInvocation(inv Invocation) error {
	return writeJSON(filepath.Join(Dir(inv.StateRoot, inv.Name), InvocationFile), inv)
}

func ReadInvocation(stateRoot, name string) (Invocation, error) {
	var inv Invocation
	if err := CheckName(name); err != nil {
		return inv, err
	}
	if err := readJSON(filepath.Join(Dir(stateRoot, name), InvocationFile), &inv); err != nil {
		return inv, fmt.Errorf("request: no invocation %s under %s: %w", name, Root(stateRoot), err)
	}
	return inv, nil
}

func WriteSummary(stateRoot string, s Summary) error {
	s.UpdatedAt = time.Now().UTC()
	return writeJSON(filepath.Join(Dir(stateRoot, s.Name), SummaryFile), s)
}

func ReadSummary(stateRoot, name string) (Summary, error) {
	var s Summary
	err := readJSON(filepath.Join(Dir(stateRoot, name), SummaryFile), &s)
	return s, err
}

func List(stateRoot string) ([]Summary, error) {
	entries, err := os.ReadDir(Root(stateRoot))
	if errors.Is(err, os.ErrNotExist) {
		return []Summary{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Summary{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := ReadSummary(stateRoot, e.Name())
		if err != nil {
			inv, ierr := ReadInvocation(stateRoot, e.Name())
			if ierr != nil {
				continue
			}
			s = Summary{Name: inv.Name, Mode: inv.Mode, Network: inv.Network, Instance: inv.Instance, CreatedAt: inv.CreatedAt,
				Message: "no summary yet: the invocation did not finish writing one"}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func SummaryOf(c *v1alpha1.ComputeContract) *ContractSummary {
	if c == nil || c.Status.Contract == nil {
		return nil
	}
	k := c.Status.Contract
	s := &ContractSummary{Phase: string(k.Phase), VMAddress: k.VMAddress}
	if k.Winner != nil {
		s.WinnerDid = k.Winner.DID
	}
	if k.Bids != nil {
		s.Bids = *k.Bids
	}
	if k.Receipt != nil {
		s.ReceiptUri = k.Receipt.URI
	}
	if k.HostKeys != nil {
		for _, h := range k.HostKeys.Keys {
			s.HostKeys = append(s.HostKeys, h.Type+" "+h.Fingerprint)
		}
	}
	if k.Release != nil {
		ok := k.Release.OK
		s.ReleaseOK = &ok
		if k.Release.Status != nil {
			s.ReleaseStatus = *k.Release.Status
		}
	}
	if k.Termination != nil {
		s.Termination = k.Termination.Reason
	}
	return s
}

type Kcp struct {
	rest      *rest.Config
	dyn       dynamic.Interface
	workspace string
}

func OpenKcp(kubeconfig, workspace string) (*Kcp, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	if workspace == "" {
		workspace = appctl.DefaultWorkspace
	}
	root := rest.CopyConfig(cfg)
	root.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + workspace
	dc, err := dynamic.NewForConfig(root)
	if err != nil {
		return nil, err
	}
	return &Kcp{rest: cfg, dyn: dc, workspace: workspace}, nil
}

func fromUnstructured(u *unstructured.Unstructured, into any) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, into)
}

func applyBody(o any, kind string) ([]byte, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(o)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(kind)
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u.MarshalJSON()
}

func (k *Kcp) ApplyRoot(ctx context.Context, spec v1alpha1.MarketRequesterSpec) error {
	body, err := applyBody(&v1alpha1.MarketRequester{ObjectMeta: metav1.ObjectMeta{Name: RootName}, Spec: spec}, "MarketRequester")
	if err != nil {
		return err
	}
	force := true
	_, err = k.dyn.Resource(appctl.MarketRequesterGVR).Patch(ctx, RootName, types.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: FieldManager, Force: &force})
	return err
}

func (k *Kcp) Root(ctx context.Context) (v1alpha1.ApplicationStatus, error) {
	u, err := k.dyn.Resource(appctl.MarketRequesterGVR).Get(ctx, RootName, metav1.GetOptions{})
	if err != nil {
		return v1alpha1.ApplicationStatus{}, err
	}
	var o v1alpha1.MarketRequester
	err = fromUnstructured(u, &o)
	return o.Status, err
}

func (k *Kcp) Contract(ctx context.Context) (*v1alpha1.ComputeContract, error) {
	u, err := k.dyn.Resource(appctl.ComputeContractGVR).Get(ctx, appctl.RequesterContract, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var o v1alpha1.ComputeContract
	if err := fromUnstructured(u, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

func until(ctx context.Context, timeout time.Duration, what string, try func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		done, err := try()
		if done {
			return err
		}
		if err != nil {
			last = err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("request: %s within %s: %w", what, timeout, last)
			}
			return fmt.Errorf("request: %s within %s", what, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(Poll):
		}
	}
}

func (k *Kcp) CreateContract(ctx context.Context, spec v1alpha1.ComputeContractSpec, timeout time.Duration) error {
	body, err := applyBody(&v1alpha1.ComputeContract{ObjectMeta: metav1.ObjectMeta{Name: appctl.RequesterContract,
		Annotations: map[string]string{v1alpha1.AnnotationTTLAfterFinished: ContractTTL}}, Spec: spec}, "ComputeContract")
	if err != nil {
		return err
	}
	var u unstructured.Unstructured
	if err := u.UnmarshalJSON(body); err != nil {
		return err
	}
	return until(ctx, timeout, "creating the ComputeContract (its schema is bound by the requester's core)", func() (bool, error) {
		_, err := k.dyn.Resource(appctl.ComputeContractGVR).Create(ctx, &u, metav1.CreateOptions{FieldManager: FieldManager})
		if err == nil || apierrors.IsAlreadyExists(err) {
			return true, nil
		}
		return false, err
	})
}

func (k *Kcp) Release(ctx context.Context) error {
	patch := []byte(`{"spec":{"release":true}}`)
	_, err := k.dyn.Resource(appctl.ComputeContractGVR).Patch(ctx, appctl.RequesterContract, types.MergePatchType, patch,
		metav1.PatchOptions{FieldManager: FieldManager})
	return err
}

func (k *Kcp) WaitContract(ctx context.Context, timeout time.Duration, done func(*v1alpha1.ComputeContract) bool,
	progress func(*v1alpha1.ComputeContract)) (*v1alpha1.ComputeContract, error) {
	var cur *v1alpha1.ComputeContract
	last := ""
	err := until(ctx, timeout, "waiting for the ComputeContract", func() (bool, error) {
		c, err := k.Contract(ctx)
		if err != nil {
			return false, err
		}
		cur = c
		if st, err := k.Root(ctx); err == nil {
			if why := requesterFailure(st); why != "" && !done(c) {
				return true, fmt.Errorf("request: the requester application cannot carry the contract: %s", why)
			}
		}
		key := string(c.Status.Phase) + "|" + c.Status.Message
		if c.Status.Contract != nil {
			key += "|" + string(c.Status.Contract.Phase)
		}
		if key != last && progress != nil {
			progress(c)
			last = key
		}
		return done(c), nil
	})
	return cur, err
}

func requesterFailure(st v1alpha1.ApplicationStatus) string {
	if st.Phase == v1alpha1.ApplicationPhaseFailed {
		return "phase Failed: " + st.Message
	}
	for _, c := range st.Components {
		if c.Phase == string(denov1alpha1.DenoPodFailed) {
			return fmt.Sprintf("%s %s/%s is Failed", c.Kind, c.Workspace, c.Name)
		}
	}
	return ""
}

func (k *Kcp) PutSecrets(ctx context.Context, bundle []byte, timeout time.Duration) (v1alpha1.VaultRef, error) {
	ref := v1alpha1.VaultRef{Mount: appctl.RequesterVaultClient, Path: SecretsPath}
	ri := k.dyn.Resource(appctl.OpenBaoServerGVR)
	err := until(ctx, timeout, "writing the secrets bundle into the requester's own OpenBao", func() (bool, error) {
		client, _, c, err := appctl.ServerClient(ctx, ri, appctl.RequesterVaultServer, appctl.RequesterVaultClient)
		if err != nil {
			return false, err
		}
		defer client.RevokeSelf(context.Background())
		ref.Mount = c.KVMount
		if err := client.PutKV(ctx, c.KVMount+"/data/"+SecretsPath, map[string]any{"bundle": string(bundle)}); err != nil {
			return false, err
		}
		return true, nil
	})
	return ref, err
}

func (k *Kcp) Identities() (*kcpclient.MarketIdentities, error) {
	c, err := kcpclient.New(kcpclient.Options{Host: k.rest.Host, RestConfig: k.rest})
	if err != nil {
		return nil, err
	}
	return c.MarketIdentities(k.workspace)
}

func (k *Kcp) WaitIdentityObject(ctx context.Context, timeout time.Duration) error {
	return until(ctx, timeout, "waiting for the requester's MarketIdentity (its core creates it)", func() (bool, error) {
		_, err := k.dyn.Resource(appctl.MarketIdentityGVR).Get(ctx, appctl.RequesterIdentity, metav1.GetOptions{})
		return err == nil, err
	})
}

func networkClient(kubeconfig string) (dynamic.ResourceInterface, error) {
	cfg, err := kcpserver.FrontRestConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.Host = strings.TrimSuffix(cfg.Host, "/") + "/clusters/" + appctl.DefaultWorkspace
	dc, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return dc.Resource(appctl.MarketNetworkGVR), nil
}

func editNetwork(ctx context.Context, kubeconfig string, edit func(*v1alpha1.MarketNetworkSpec) error) error {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return err
	}
	for range networkConflicts {
		u, err := ri.Get(ctx, appctl.NetworkName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("request: the market network's MarketNetwork: %w", err)
		}
		var o v1alpha1.MarketNetwork
		if err := fromUnstructured(u, &o); err != nil {
			return err
		}
		if err := edit(&o.Spec); err != nil {
			return err
		}
		apps, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&o.Spec)
		if err != nil {
			return err
		}
		u.Object["spec"] = apps
		_, err = ri.Update(ctx, u, metav1.UpdateOptions{FieldManager: FieldManager})
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return errors.New("request: the MarketNetwork stayed in conflict")
}

func AddToNetwork(ctx context.Context, kubeconfig string, app v1alpha1.NetworkApp) error {
	return editNetwork(ctx, kubeconfig, func(s *v1alpha1.MarketNetworkSpec) error {
		for _, a := range s.Apps {
			if a.Name == app.Name {
				return fmt.Errorf("request: the market network already has an application %s", app.Name)
			}
		}
		s.Apps = append(s.Apps, app)
		return nil
	})
}

func SetStopped(ctx context.Context, kubeconfig, name string, stopped bool) error {
	return editNetwork(ctx, kubeconfig, func(s *v1alpha1.MarketNetworkSpec) error {
		for i := range s.Apps {
			if s.Apps[i].Name == name {
				s.Apps[i].Stopped = stopped
				return nil
			}
		}
		return fmt.Errorf("request: the market network has no application %s", name)
	})
}

func RemoveFromNetwork(ctx context.Context, kubeconfig, name string) error {
	return editNetwork(ctx, kubeconfig, func(s *v1alpha1.MarketNetworkSpec) error {
		var keep []v1alpha1.NetworkApp
		for _, a := range s.Apps {
			if a.Name != name {
				keep = append(keep, a)
			}
		}
		s.Apps = keep
		return nil
	})
}

func NetworkApp(ctx context.Context, kubeconfig, name string) (v1alpha1.NetworkAppStatus, bool, error) {
	ri, err := networkClient(kubeconfig)
	if err != nil {
		return v1alpha1.NetworkAppStatus{}, false, err
	}
	u, err := ri.Get(ctx, appctl.NetworkName, metav1.GetOptions{})
	if err != nil {
		return v1alpha1.NetworkAppStatus{}, false, err
	}
	var o v1alpha1.MarketNetwork
	if err := fromUnstructured(u, &o); err != nil {
		return v1alpha1.NetworkAppStatus{}, false, err
	}
	for _, a := range o.Status.Apps {
		if a.Name == name {
			return a, true, nil
		}
	}
	return v1alpha1.NetworkAppStatus{}, false, nil
}

func WaitNetworkApp(ctx context.Context, kubeconfig, name string, timeout time.Duration, done func(v1alpha1.NetworkAppStatus) bool,
	progress func(v1alpha1.NetworkAppStatus)) (v1alpha1.NetworkAppStatus, error) {
	var cur v1alpha1.NetworkAppStatus
	last := ""
	err := until(ctx, timeout, "waiting for the market network to place "+name, func() (bool, error) {
		a, found, err := NetworkApp(ctx, kubeconfig, name)
		if err != nil || !found {
			return false, err
		}
		cur = a
		if key := string(a.Phase) + a.Message; key != last && progress != nil {
			progress(a)
			last = key
		}
		if a.Phase == v1alpha1.ApplicationPhaseFailed {
			return true, fmt.Errorf("request: the market network failed %s: %s", name, a.Message)
		}
		return done(a), nil
	})
	return cur, err
}

func HostKeyAlias(receiptURI string) string {
	clean := func(v string) string {
		v = strings.ToLower(v)
		var b strings.Builder
		dash := false
		for _, r := range v {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
				dash = false
				continue
			}
			if !dash {
				b.WriteByte('-')
				dash = true
			}
		}
		return strings.Trim(b.String(), "-")
	}
	rest := strings.TrimPrefix(receiptURI, "at://")
	authority, _, _ := strings.Cut(rest, "/")
	rkey := receiptURI[strings.LastIndex(receiptURI, "/")+1:]
	return "swc-guest-" + clean(authority) + "-" + clean(rkey)
}

func Ticket(vmAddress string) (string, error) {
	if !strings.HasPrefix(vmAddress, irohScheme) {
		return "", fmt.Errorf("request: the guest address %q is not an iroh ticket; iroh is the only guest transport", vmAddress)
	}
	return strings.TrimPrefix(vmAddress, irohScheme), nil
}
