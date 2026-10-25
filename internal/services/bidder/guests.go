package bidder

import (
	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/computevm"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
	core "github.com/johnandersen777/socialweb-computer-kcp/internal/market/bidder"
)

type guests struct{}

var _ core.Guests = guests{}

func (guests) AcceptKey(uri, cid string) string {
	return computevm.AcceptRef{URI: uri, CID: cid}.Key()
}

func (guests) AcceptKeyOf(vm *v1alpha1.ComputeVM) string { return computevm.AcceptKeyOf(vm) }

func (guests) Ended(vm *v1alpha1.ComputeVM) bool { return computevm.Ended(vm) }

func (guests) Releasing(vm *v1alpha1.ComputeVM) bool { return computevm.Releasing(vm) }

func (guests) ReleaseRequested(vm *v1alpha1.ComputeVM) bool { return computevm.ReleaseRequested(vm) }

func (guests) ReleaseRequestRef(vm *v1alpha1.ComputeVM) *commonv1.StrongRef {
	return computevm.ReleaseRequestRef(vm)
}

func (guests) GuestReport(vm *v1alpha1.ComputeVM) *v1alpha1.ComputeVMGuestReport {
	return computevm.GuestReportOf(vm)
}

func (guests) Published(vm *v1alpha1.ComputeVM) *v1alpha1.ComputeVMPublished {
	return computevm.PublishedOf(vm)
}

func (guests) ComputeVM(in core.GuestVM) (*v1alpha1.ComputeVM, error) {
	input := computevm.VMInput{
		Name:             in.Name,
		Handle:           in.Handle,
		VM:               in.Shape,
		RequesterDID:     in.RequesterDID,
		Vault:            computevm.GuestVaultRef{Server: in.VaultServer, Mount: in.VaultMount},
		StopGraceSeconds: in.StopGraceSeconds,
		GuestLogin:       in.Login,
		HostPorts:        in.HostPorts,
		LANUDP:           in.LANUDP,
		HostUDP:          in.HostUDP,
		WebRTCUDPPort:    in.WebRTCUDPPort,
		Owner:            in.Owner,
	}
	if in.Accept.URI != "" || in.Accept.CID != "" {
		input.Accept = &computevm.AcceptRef{URI: in.Accept.URI, CID: in.Accept.CID}
	}
	return computevm.ComputeVMFor(input)
}

func (guests) UserData(name, userData string) (string, map[string]any) {
	return computevm.UserDataPath(name), computevm.UserDataEntry(userData)
}

func (guests) LoginSecret(acceptKey string) (string, string, error) {
	if acceptKey == "" {
		return computevm.GuestLoginSecret()
	}
	secret, hash := computevm.GuestLoginSecretFor(acceptKey)
	return secret, hash, nil
}

func (guests) Audience(name string) string {
	return guestidentity.GuestAudience(guestidentity.KCPAPI, name)
}

func (guests) Grant(bidderDID, requesterDID, role, name string) (v1alpha1.GuestIdentityGrant, error) {
	subject, err := guestidentity.SubjectFor(bidderDID, requesterDID, role)
	if err != nil {
		return v1alpha1.GuestIdentityGrant{}, err
	}
	return guestidentity.GrantFor(guestidentity.GrantSubject{ComputeVM: name, RequesterDID: requesterDID, Subject: subject},
		guestidentity.OwnWish(name)), nil
}

func (guests) BidConfig(issuerURL, bidderDID, acceptPath, createdAt string) (string, any, error) {
	config, err := guestidentity.BidConfigFor(issuerURL, bidderDID, acceptPath, createdAt)
	return guestidentity.WIFSimpleNSID, config, err
}

func (guests) Termination(vm *v1alpha1.ComputeVM, workspace, reason, observedAt string, request *commonv1.StrongRef) (v1alpha1.StateSync, error) {
	object, err := computevm.TerminationStateSync(vm, workspace, reason, observedAt, request)
	if err != nil {
		return v1alpha1.StateSync{}, err
	}
	return *object, nil
}

func (guests) EventAdapter() string { return computevm.TerminationSyncAdapter }

func (guests) SyncLabels(vm *v1alpha1.ComputeVM) map[string]string {
	return map[string]string{
		computevm.TerminationSyncSourceLabel: computevm.TerminationSyncSource,
		computevm.TerminationSyncUIDLabel:    string(vm.UID),
		computevm.TerminationSyncVMNameLabel: vm.Name,
	}
}

func (guests) SyncVM(sync *v1alpha1.StateSync) string {
	return sync.Labels[computevm.TerminationSyncVMNameLabel]
}

func (guests) CheckGuestEvent(record map[string]any) (string, error) {
	return guestidentity.CheckGuestEvent(record)
}

func (guests) NetworkKind(tag, address string) string {
	return guestidentity.NetworkKind(tag, address)
}
