package apiresource

import (
	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
)

type PrinterColumn struct {
	Name string

	Type string

	JSONPath string
}

type APIResource struct {
	Kind string

	Plural string

	Revision int

	Export string

	Object any

	PrinterColumns []PrinterColumn

	Contracts []string
}

func (r APIResource) ContractLabels() map[string]string {
	return commonv1.ContractLabels(r.Contracts...)
}
