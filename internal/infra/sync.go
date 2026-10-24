package infra

import "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"

type Action string

const (
	ActionInSync Action = "InSync"

	ActionCreate Action = "Create"

	ActionPush Action = "Push"

	ActionPull Action = "Pull"

	ActionConflict Action = "Conflict"

	ActionDelete Action = "Delete"

	ActionInvalid Action = "Invalid"
)

type Record struct {
	Present bool

	CID string

	Hash string

	Err error
}

type SyncInput struct {
	Follow bool

	Stored *v1alpha1.InfraRecordStatus

	URI string

	SpecHash string

	Record Record
}

func Decide(in SyncInput) Action {
	var stored v1alpha1.InfraRecordStatus
	if in.Stored != nil && in.Stored.URI == in.URI {
		stored = *in.Stored
	}
	if !in.Record.Present {
		if in.Follow || stored.CID != "" {
			return ActionDelete
		}
		return ActionCreate
	}
	if in.Record.Err != nil {
		return ActionInvalid
	}
	if in.Record.Hash == in.SpecHash {
		return ActionInSync
	}
	if stored.DeclarationHash == "" {
		if in.Follow {
			return ActionPull
		}
		return ActionConflict
	}
	kcpChanged := in.SpecHash != stored.DeclarationHash
	recordChanged := in.Record.Hash != stored.DeclarationHash
	switch {
	case kcpChanged && recordChanged:
		return ActionConflict
	case recordChanged:
		return ActionPull
	}
	return ActionPush
}
