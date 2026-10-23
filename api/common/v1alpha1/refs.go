package v1alpha1

type ObjectRef struct {
	Group string `json:"group,omitempty"`

	Kind string `json:"kind" apigen:"maxLength=63"`

	Name string `json:"name" apigen:"maxLength=253"`

	Workspace string `json:"workspace,omitempty"`
}

type LocalObjectRef struct {
	Name string `json:"name" apigen:"maxLength=253"`
}

type StrongRef struct {
	URI string `json:"uri"`

	CID string `json:"cid"`
}

type SecretRef struct {
	Vault *ObjectRef `json:"vault,omitempty"`

	Mount string `json:"mount"`

	Path string `json:"path"`

	Key string `json:"key,omitempty"`
}

type CredentialsRef struct {
	ObjectRef `json:",inline"`
}

func (r ObjectRef) IsZero() bool {
	return r.Kind == "" && r.Name == ""
}

func (r ObjectRef) InWorkspace(fallback string) string {
	if r.Workspace != "" {
		return r.Workspace
	}
	return fallback
}

func (r ObjectRef) String() string {
	s := r.Kind
	if r.Group != "" {
		s += "." + r.Group
	}
	s += "/" + r.Name
	if r.Workspace != "" {
		s = r.Workspace + ":" + s
	}
	return s
}
