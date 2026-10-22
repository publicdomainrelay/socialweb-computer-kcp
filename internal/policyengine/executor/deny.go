package executor

import (
	"context"
)

type DenyExecutor struct {
	RecordType string
	Message    string
}

func NewDeny(recordType, message string) *DenyExecutor {
	return &DenyExecutor{RecordType: recordType, Message: message}
}

func (d *DenyExecutor) Kind() string {
	return d.RecordType
}

func (d *DenyExecutor) Execute(ctx context.Context, record PolicyRecord, eval *EvalContext, permissions map[string]any) (Verdict, error) {
	return Deny(d.Message, d.RecordType), nil
}

func (d *DenyExecutor) Scope(ctx context.Context, record PolicyRecord, input ScopeInput) (Verdict, bool, error) {
	return Deny(d.Message, d.RecordType), true, nil
}
