package v1alpha1

type AssociationPolicy struct {
	MaxDepth int32 `json:"maxDepth,omitempty" apigen:"minimum=1,maximum=100"`

	RequireContract bool `json:"requireContract,omitempty"`
}
