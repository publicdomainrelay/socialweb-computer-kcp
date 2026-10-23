package policyaction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/common/ref"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kcplibs/impl/kcpstore"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/policyengine/controller"
)

var (
	ErrNoStore = errors.New("policyaction: a kcp store is required")

	ErrNoSpec = errors.New("policyaction: the ComputeVM action spec is required")
)

type ComputeVMSpec struct {
	Workspace string `json:"workspace,omitempty"`

	Name string `json:"name"`

	Owner string `json:"owner"`

	Contract *v1alpha1.ComputeVMContract `json:"contract,omitempty"`

	VM v1alpha1.ComputeVMShape `json:"vm"`

	Requester v1alpha1.ComputeVMRequester `json:"requester"`

	Vault ComputeVMVaultRef `json:"vault"`

	ReuseStale bool `json:"reuseStale,omitempty"`

	StopGraceSeconds int32 `json:"stopGraceSeconds,omitempty"`

	GuestLogin *v1alpha1.ComputeVMGuestLogin `json:"guestLogin,omitempty"`

	Accept *ComputeVMAcceptRef `json:"accept,omitempty"`
}

type ComputeVMVaultRef struct {
	Server string `json:"server"`

	Mount string `json:"mount"`
}

type ComputeVMAcceptRef struct {
	Uri string `json:"uri"`

	Cid string `json:"cid"`
}

type ComputeVMHandler struct {
	Store *kcpstore.Store

	Workspace string
}

func NewComputeVMHandler(store *kcpstore.Store, workspace string) (*ComputeVMHandler, error) {
	if store == nil {
		return nil, ErrNoStore
	}
	return &ComputeVMHandler{Store: store, Workspace: strings.TrimSpace(workspace)}, nil
}

func (h *ComputeVMHandler) Kind() string {
	return controller.TriggerActionComputeVM
}

func (h *ComputeVMHandler) Dispatch(ctx context.Context, spec json.RawMessage, _ controller.Outcome) error {
	if h == nil || h.Store == nil {
		return ErrNoStore
	}
	decoded, err := decodeComputeVMSpec(spec)
	if err != nil {
		return err
	}
	workspace := strings.TrimSpace(decoded.Workspace)
	if workspace == "" {
		workspace = strings.TrimSpace(h.Workspace)
	}
	if err := validateWorkspace(workspace); err != nil {
		return err
	}
	name := strings.TrimSpace(decoded.Name)
	if name == "" {
		return errors.New("policyaction: the ComputeVM action spec names no name")
	}
	owner := strings.TrimSpace(decoded.Owner)
	if owner == "" {
		return errors.New("policyaction: the ComputeVM action spec names no owner")
	}
	input := computevm.VMInput{
		Name:             name,
		Handle:           owner,
		VM:               decoded.VM,
		RequesterDID:     strings.TrimSpace(decoded.Requester.Did),
		Vault:            computevm.GuestVaultRef{Server: strings.TrimSpace(decoded.Vault.Server), Mount: strings.TrimSpace(decoded.Vault.Mount)},
		ReuseStale:       decoded.ReuseStale,
		StopGraceSeconds: decoded.StopGraceSeconds,
		GuestLogin:       decoded.GuestLogin,
		Contract:         decoded.Contract,
	}
	if decoded.Accept != nil {
		input.Accept = &computevm.AcceptRef{URI: strings.TrimSpace(decoded.Accept.Uri), CID: strings.TrimSpace(decoded.Accept.Cid)}
	}
	if err := computevm.ValidateVMInput(input); err != nil {
		return fmt.Errorf("policyaction: the ComputeVM action spec: %w", err)
	}
	vm := &v1alpha1.ComputeVM{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM"},
		ObjectMeta: metav1.ObjectMeta{Name: input.Name},
		Spec: v1alpha1.ComputeVMSpec{
			BackendRef: computevm.BackendRef(input.Name),
			VM:         input.VM,
			Owner:      v1alpha1.ComputeVMOwner{Handle: input.Handle},
			UserData:   v1alpha1.ComputeVMUserData{Vault: computevm.UserDataRef(input.Name, input.Vault)},
			Requester:  v1alpha1.ComputeVMRequester{Did: input.RequesterDID, Plc: strings.TrimSpace(decoded.Requester.Plc)},
		},
	}
	if input.Accept != nil && input.Accept.Key() != "" {
		vm.Spec.Accept = &commonv1.StrongRef{URI: input.Accept.URI, CID: input.Accept.CID}
	}
	if input.Contract != nil {
		contract := *input.Contract
		vm.Spec.Contract = &contract
	}
	if input.ReuseStale {
		vm.Spec.Image = &v1alpha1.ComputeVMImageRequest{ReuseStale: true}
	}
	if input.StopGraceSeconds > 0 {
		vm.Spec.StopGraceSeconds = input.StopGraceSeconds
	}
	if input.GuestLogin != nil {
		login := *input.GuestLogin
		vm.Spec.GuestLogin = &login
	}
	return h.ensure(ctx, workspace, vm)
}

func (h *ComputeVMHandler) DispatchComputeVM(ctx context.Context, action policyv1alpha1.PolicyAction, outcome controller.Outcome) error {
	return h.Dispatch(ctx, action.Spec, outcome)
}

func (h *ComputeVMHandler) ensure(ctx context.Context, workspace string, vm *v1alpha1.ComputeVM) error {
	resource := kcpstore.OfClusterScoped[v1alpha1.ComputeVM](h.Store, computevm.GVR)
	target := ref.New(workspace, "", vm.Name)
	existing, err := resource.Get(ctx, target)
	if err == nil {
		return ensureExisting(ctx, resource, target, existing, vm)
	}
	if !kcpstore.IsNotFound(err) {
		return fmt.Errorf("policyaction: read ComputeVM %s in %s: %w", vm.Name, workspace, err)
	}
	if err := resource.Create(ctx, workspace, vm); err != nil {
		return fmt.Errorf("policyaction: create ComputeVM %s in %s: %w", vm.Name, workspace, err)
	}
	return nil
}

func ensureExisting(ctx context.Context, resource *kcpstore.Resource[v1alpha1.ComputeVM], target ref.Ref, existing, wanted *v1alpha1.ComputeVM) error {
	if existing.DeletionTimestamp != nil {
		return fmt.Errorf("policyaction: ComputeVM %s in %s is terminating", existing.Name, target.LogicalCluster)
	}
	if existing.Spec.Owner.Handle != wanted.Spec.Owner.Handle {
		return fmt.Errorf("policyaction: ComputeVM %s in %s belongs to owner %q, not %q",
			existing.Name, target.LogicalCluster, existing.Spec.Owner.Handle, wanted.Spec.Owner.Handle)
	}
	if wanted.Spec.Contract == nil {
		return nil
	}
	if existing.Spec.Contract != nil {
		if *existing.Spec.Contract == *wanted.Spec.Contract {
			return nil
		}
		return fmt.Errorf("policyaction: ComputeVM %s in %s already carries a different contract", existing.Name, target.LogicalCluster)
	}
	raw, err := json.Marshal(wanted.Spec.Contract)
	if err != nil {
		return fmt.Errorf("policyaction: encode the contract of ComputeVM %s: %w", existing.Name, err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("policyaction: encode the contract of ComputeVM %s: %w", existing.Name, err)
	}
	patch, err := json.Marshal([]map[string]any{{"op": "add", "path": "/spec/contract", "value": value}})
	if err != nil {
		return fmt.Errorf("policyaction: encode the contract patch of ComputeVM %s: %w", existing.Name, err)
	}
	if err := resource.PatchJSON(ctx, target, patch); err != nil {
		return fmt.Errorf("policyaction: set the contract of ComputeVM %s in %s: %w", existing.Name, target.LogicalCluster, err)
	}
	return nil
}

func decodeComputeVMSpec(raw json.RawMessage) (ComputeVMSpec, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ComputeVMSpec{}, ErrNoSpec
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ComputeVMSpec{}, ErrNoSpec
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var spec ComputeVMSpec
	if err := decoder.Decode(&spec); err != nil {
		return ComputeVMSpec{}, fmt.Errorf("policyaction: decode the ComputeVM action spec: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return ComputeVMSpec{}, errors.New("policyaction: the ComputeVM action spec holds more than one JSON value")
		}
		return ComputeVMSpec{}, fmt.Errorf("policyaction: decode the ComputeVM action spec: %w", err)
	}
	return spec, nil
}

func validateWorkspace(workspace string) error {
	switch {
	case workspace == "":
		return errors.New("policyaction: the ComputeVM action names no target workspace")
	case workspace == "." || workspace == "..":
		return fmt.Errorf("policyaction: %q is not a target workspace", workspace)
	case strings.ContainsAny(workspace, "/\\"):
		return fmt.Errorf("policyaction: %q is not a target workspace", workspace)
	}
	for _, r := range workspace {
		if r <= ' ' || r == 127 {
			return fmt.Errorf("policyaction: %q is not a target workspace", workspace)
		}
	}
	return nil
}
