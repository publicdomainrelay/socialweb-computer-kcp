package computevm

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

const (
	GuestLoginDefaultMount = "sshsig"

	GuestLoginSecretBytes = 32
)

var (
	guestNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

	guestMountRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

	bindHashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type AcceptRef struct {
	URI string

	CID string
}

func (a AcceptRef) Key() string {
	if a.URI == "" || a.CID == "" {
		return ""
	}
	return a.URI + "#" + a.CID
}

type GuestVaultRef struct {
	Server string

	Mount string
}

type VMInput struct {
	Name string

	Handle string

	VM v1alpha1.ComputeVMShape

	RequesterDID string

	Vault GuestVaultRef

	ReuseStale bool

	StopGraceSeconds int32

	GuestLogin *v1alpha1.ComputeVMGuestLogin

	Accept *AcceptRef

	Contract *v1alpha1.ComputeVMContract

	HostPorts []int32

	LANUDP bool

	HostUDP bool

	WebRTCUDPPort int32

	Owner *metav1.OwnerReference
}

func UserDataPath(name string) string {
	return v1alpha1.GuestVaultPrefix + "/" + name + "/" + v1alpha1.GuestUserDataEntry
}

func BackendRef(name string) commonv1.ObjectRef {
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: v1alpha1.BackendKindFirecrackerVM, Name: name}
}

func UserDataRef(name string, vault GuestVaultRef) commonv1.SecretRef {
	return commonv1.SecretRef{Vault: v1alpha1.VaultServerRef(vault.Server), Mount: vault.Mount, Path: UserDataPath(name)}
}

func UserDataEntry(userData string) map[string]any {
	return map[string]any{v1alpha1.GuestUserDataKey: userData}
}

func GuestLoginSecret() (string, string, error) {
	raw := make([]byte, GuestLoginSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("computevm: drawing a guest bind secret: %w", err)
	}
	secret, hash := GuestLoginSecretFor(hex.EncodeToString(raw))
	return secret, hash, nil
}

func GuestLoginSecretFor(seed string) (string, string) {
	sum := sha256.Sum256([]byte(seed))
	secret := hex.EncodeToString(sum[:])
	hashSum := sha256.Sum256([]byte(secret))
	return secret, hex.EncodeToString(hashSum[:])
}

func ComputeVMFor(in VMInput) (*v1alpha1.ComputeVM, error) {
	if err := ValidateVMInput(in); err != nil {
		return nil, err
	}
	o := &v1alpha1.ComputeVM{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ComputeVM"},
		ObjectMeta: metav1.ObjectMeta{Name: in.Name},
		Spec: v1alpha1.ComputeVMSpec{
			BackendRef: BackendRef(in.Name),
			VM:         in.VM,
			Owner:      v1alpha1.ComputeVMOwner{Handle: in.Handle},
			UserData:   v1alpha1.ComputeVMUserData{Vault: UserDataRef(in.Name, in.Vault)},
			Requester:  v1alpha1.ComputeVMRequester{Did: in.RequesterDID},
		},
	}
	if in.Accept != nil && in.Accept.Key() != "" {
		o.Spec.Accept = &commonv1.StrongRef{URI: in.Accept.URI, CID: in.Accept.CID}
	}
	if in.Owner != nil {
		o.OwnerReferences = []metav1.OwnerReference{*in.Owner}
	}
	if in.Contract != nil {
		contract := *in.Contract
		o.Spec.Contract = &contract
	}
	if in.ReuseStale {
		o.Spec.Image = &v1alpha1.ComputeVMImageRequest{ReuseStale: true}
	}
	if in.StopGraceSeconds < 0 {
		return nil, errors.New("computevm: spec.stopGraceSeconds is negative")
	}
	if in.StopGraceSeconds > 0 {
		o.Spec.StopGraceSeconds = in.StopGraceSeconds
	}
	if in.GuestLogin != nil {
		login := *in.GuestLogin
		o.Spec.GuestLogin = &login
	}
	for _, p := range in.HostPorts {
		if p <= 0 || p > 65535 {
			return nil, fmt.Errorf("computevm: host port %d is not a TCP port", p)
		}
		if !slices.Contains(o.Spec.HostPorts, p) {
			o.Spec.HostPorts = append(o.Spec.HostPorts, p)
		}
	}
	slices.Sort(o.Spec.HostPorts)
	o.Spec.GuestNetwork = v1alpha1.GuestNetworkWith(in.LANUDP, in.HostUDP, in.WebRTCUDPPort)
	return o, nil
}

func ValidateVMInput(in VMInput) error {
	switch {
	case len(in.Name) > 201 || !guestNameRE.MatchString(in.Name):
		return fmt.Errorf("computevm: %q is not a ComputeVM name", in.Name)
	case in.Handle == "":
		return errors.New("computevm: the VM names no owner handle")
	case in.VM.Role == "":
		return errors.New("computevm: the RFP names no vm.role, and the guest identity subject and market tags are built from it")
	case in.VM.Cpus < 0:
		return errors.New("computevm: spec.vm.cpus is negative")
	case !strings.HasPrefix(in.RequesterDID, "did:"):
		return fmt.Errorf("computevm: requester %q is not a DID", in.RequesterDID)
	case in.Vault.Server == "":
		return errors.New("computevm: the VM names no vault server for its user data")
	case strings.Trim(in.Vault.Mount, "/") == "":
		return errors.New("computevm: the VM names no vault mount for its user data")
	}
	if in.Accept != nil && in.Accept.Key() == "" {
		return errors.New("computevm: the accept names no uri and cid together")
	}
	if err := checkGuestLogin(in.GuestLogin); err != nil {
		return err
	}
	return checkContract(in.Contract)
}

func ValidateComputeVM(o *v1alpha1.ComputeVM) error {
	if o == nil {
		return errors.New("computevm: no ComputeVM")
	}
	switch {
	case len(o.Name) > 201 || !guestNameRE.MatchString(o.Name):
		return fmt.Errorf("computevm: %q is not a ComputeVM name", o.Name)
	case o.Spec.Owner.Handle == "":
		return errors.New("computevm: spec.owner.handle is empty")
	case o.Spec.VM.Role == "":
		return errors.New("computevm: spec.vm.role is empty")
	case o.Spec.VM.Cpus < 0:
		return errors.New("computevm: spec.vm.cpus is negative")
	case v1alpha1.VaultServer(o.Spec.UserData.Vault.Vault, "") == "":
		return errors.New("computevm: spec.userData.vault.server is empty")
	case strings.Trim(o.Spec.UserData.Vault.Mount, "/") == "":
		return errors.New("computevm: spec.userData.vault.mount is empty")
	case strings.Trim(o.Spec.UserData.Vault.Path, "/") == "" || strings.Contains(o.Spec.UserData.Vault.Path, ".."):
		return errors.New("computevm: spec.userData.vault.path is empty or climbs")
	case !strings.HasPrefix(o.Spec.Requester.Did, "did:"):
		return fmt.Errorf("computevm: spec.requester.did %q is not a DID", o.Spec.Requester.Did)
	case o.Spec.StopGraceSeconds < 0:
		return errors.New("computevm: spec.stopGraceSeconds is negative")
	}
	if err := checkGuestLogin(o.Spec.GuestLogin); err != nil {
		return err
	}
	return checkContract(o.Spec.Contract)
}

func checkGuestLogin(login *v1alpha1.ComputeVMGuestLogin) error {
	if login == nil {
		return nil
	}
	if len(login.Mount) > 63 || !guestMountRE.MatchString(login.Mount) {
		return fmt.Errorf("computevm: guest login mount %q is not an auth mount name", login.Mount)
	}
	if !bindHashRE.MatchString(login.BindSecretSha256) {
		return errors.New("computevm: guest login bindSecretSha256 is not a lowercase hex sha256")
	}
	return nil
}

func checkContract(contract *v1alpha1.ComputeVMContract) error {
	if contract == nil {
		return nil
	}
	if (contract.Accept.URI == "") != (contract.Accept.CID == "") {
		return errors.New("computevm: spec.contract names an accept uri and cid together or neither")
	}
	if (contract.Receipt.URI == "") != (contract.Receipt.CID == "") {
		return errors.New("computevm: spec.contract names a receipt uri and cid together or neither")
	}
	return nil
}

func AcceptKeyOf(o *v1alpha1.ComputeVM) string {
	if o == nil || o.Spec.Accept == nil {
		return ""
	}
	return AcceptRef{URI: o.Spec.Accept.URI, CID: o.Spec.Accept.CID}.Key()
}

func ReleaseRequested(o *v1alpha1.ComputeVM) bool {
	return o != nil && o.Spec.Release != nil
}

func ReleaseRequestedAt(o *v1alpha1.ComputeVM) string {
	if o == nil || o.Spec.Release == nil {
		return ""
	}
	return strings.TrimSpace(o.Spec.Release.RequestedAt)
}

func ReleaseRequestRef(o *v1alpha1.ComputeVM) *commonv1.StrongRef {
	if o == nil || o.Spec.Release == nil || o.Spec.Release.Request == nil || strings.TrimSpace(o.Spec.Release.Request.URI) == "" {
		return nil
	}
	request := *o.Spec.Release.Request
	return &request
}

func ContractReceipt(o *v1alpha1.ComputeVM) string {
	if o == nil || o.Spec.Contract == nil {
		return ""
	}
	return o.Spec.Contract.Receipt.URI
}

func Ended(o *v1alpha1.ComputeVM) bool {
	return guestidentity.Ended(o)
}

func Releasing(o *v1alpha1.ComputeVM) bool {
	return guestidentity.Releasing(o)
}

func GoneReason(lost v1alpha1.ComputeVMLostReason) string {
	switch lost {
	case v1alpha1.ComputeVMLostHostRebooted:
		return "host_reboot"
	case v1alpha1.ComputeVMLostOOMKilled:
		return "oom"
	case v1alpha1.ComputeVMLostIdentityMismatch:
		return "identity_mismatch"
	case v1alpha1.ComputeVMLostGuestPoweroff:
		return "guest_poweroff"
	default:
		return "vmm_exited"
	}
}
