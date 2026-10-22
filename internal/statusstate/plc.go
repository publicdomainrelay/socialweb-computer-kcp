package statusstate

import (
	"context"
	"errors"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/plc"
)

func (c *Controller) reconcilePLC(ctx context.Context, dyn dynamic.Interface, u *unstructured.Unstructured) {
	var obj v1alpha1.PlcIdentity
	if err := decode(u, &obj); err != nil {
		c.log.Warn("decoding a PlcIdentity", "workspace", c.workspace, "name", u.GetName(), "err", err.Error())
		return
	}
	generation := u.GetGeneration()
	status := obj.Status
	status.ObservedGeneration = generation
	did := strings.TrimSpace(obj.Spec.DID)
	if did == "" {
		setReady(&status.Conditions, generation, false, ReasonWaiting, "the PlcIdentity needs a did")
		c.write(ctx, dyn, PlcIdentityGVR, u, &status)
		return
	}
	if c.plc == nil {
		setReady(&status.Conditions, generation, false, ReasonSourceUnavailable, "no PLC client is configured")
		c.write(ctx, dyn, PlcIdentityGVR, u, &status)
		return
	}
	entries, err := c.plc.AuditLog(ctx, did)
	if err != nil {
		var plcErr *plc.Error
		if errors.As(err, &plcErr) && plcErr.Status == 404 {
			status.Head = ""
			status.OpCount = 0
			status.Tombstoned = false
			setReady(&status.Conditions, generation, false, ReasonNotFound, "the PLC directory does not know "+did)
			c.write(ctx, dyn, PlcIdentityGVR, u, &status)
			return
		}
		setReady(&status.Conditions, generation, false, ReasonSourceError, "reading the PLC log: "+err.Error())
		c.write(ctx, dyn, PlcIdentityGVR, u, &status)
		return
	}
	var head string
	var count int64
	var tombstoned bool
	for i := range entries {
		if entries[i].Nullified {
			continue
		}
		count++
		head = entries[i].CID
		tombstoned = entries[i].Operation.Type == plc.TypeTombstone
	}
	if count == 0 {
		status.Head = ""
		status.OpCount = 0
		status.Tombstoned = false
		setReady(&status.Conditions, generation, false, ReasonNotFound, "the PLC directory holds no current operation for "+did)
		c.write(ctx, dyn, PlcIdentityGVR, u, &status)
		return
	}
	status.Head = head
	status.OpCount = count
	status.Tombstoned = tombstoned
	setReady(&status.Conditions, generation, true, ReasonReady, "the PLC identity has current operations")
	c.write(ctx, dyn, PlcIdentityGVR, u, &status)
}
