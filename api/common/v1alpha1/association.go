package v1alpha1

const (
	AssociationKindClaim       = "claim"
	AssociationKindAssociation = "association"
	AssociationKindBskyPost    = "bskyPost"
	AssociationKindDelvePost   = "delvePost"

	DefaultAssociationPostTemplate = "associate {did}"
)

type AssociationPolicy struct {
	Kinds []string `json:"kinds,omitempty" apigen:"listType=set,enum=claim|association|bskyPost|delvePost"`

	PostTemplate string `json:"postTemplate,omitempty"`

	MaxDepth int32 `json:"maxDepth,omitempty" apigen:"minimum=1,maximum=1"`

	RequireContract bool `json:"requireContract,omitempty"`
}
