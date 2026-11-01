package openbao

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/guestidentity"
)

type vmStore struct {
	p *Plane
}

var _ guestidentity.VMStore = vmStore{}

func (v vmStore) Get(_ context.Context, name string) (*v1alpha1.ComputeVM, error) {
	return v.p.vm(name), nil
}

func (v vmStore) RecordGuest(ctx context.Context, name string, report v1alpha1.ComputeVMGuestReport) error {
	if report.Address == "" && len(report.HostKeys) == 0 && report.ReportedAt == "" {
		return nil
	}
	current := v.p.vm(name)
	if current == nil {
		return guestidentity.ErrUnknownGuest
	}
	if held := guestidentity.GuestReportOf(current); held != nil && reflect.DeepEqual(*held, report) {
		return nil
	}
	return v.p.deps.Guests.RecordGuest(ctx, name, report)
}

func (p *Plane) withGuestLogin(reply guestidentity.Reply) guestidentity.Reply {
	if reply.Status != http.StatusOK {
		return reply
	}
	body := map[string]any{}
	if err := json.Unmarshal(reply.Body, &body); err != nil {
		return reply
	}
	name, _ := body["name"].(string)
	if name == "" {
		return reply
	}
	vm := p.vm(name)
	if vm == nil {
		return reply
	}
	ref, err := guestidentity.GuestLoginRef(*vm, nil)
	if err != nil {
		return reply
	}
	body["guestLogin"] = ref
	raw, err := json.Marshal(body)
	if err != nil {
		return reply
	}
	reply.Body = raw
	return reply
}
