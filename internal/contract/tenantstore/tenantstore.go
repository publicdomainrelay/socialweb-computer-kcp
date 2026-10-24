package tenantstore

import "context"

type ServiceAccount struct {
	Namespace string
	Name      string
}

type Erased struct {
	ServiceAccount bool
	Secrets        []string
}

type Eraser interface {
	EraseServiceAccount(ctx context.Context, endpoint string, sa ServiceAccount) (Erased, error)
}
