package scale

type OperatorKind struct {
	Group string

	Kind string

	ReadyCondition string

	InCluster bool
}

type Operators []OperatorKind

var OurOperators = Operators{}

type CustomResourceCondition struct {
	Type string

	Status string

	ObservedGeneration int64
}

type CustomResource struct {
	Group string

	Kind string

	Namespace string

	Name string

	Generation int64

	ObservedGeneration int64

	Deleting bool

	Finalizers int

	Conditions []CustomResourceCondition
}

func (r Operators) InCluster() Operators {
	var out Operators
	for _, k := range r {
		if k.InCluster {
			out = append(out, k)
		}
	}
	return out
}

func (r Operators) lookup(group, kind string) (OperatorKind, bool) {
	for _, k := range r {
		if k.Group == group && k.Kind == kind {
			return k, true
		}
	}
	return OperatorKind{}, false
}

func (r Operators) Unreconciled(crs []CustomResource) []CustomResource {
	var out []CustomResource
	for _, cr := range crs {
		k, ok := r.lookup(cr.Group, cr.Kind)
		if !ok || !k.InCluster {
			continue
		}
		if !k.Reconciled(cr) {
			out = append(out, cr)
		}
	}
	return out
}

func (k OperatorKind) Reconciled(cr CustomResource) bool {
	if cr.Deleting {
		return cr.Finalizers == 0
	}
	if cr.ObservedGeneration != 0 && cr.ObservedGeneration < cr.Generation {
		return false
	}
	for _, c := range cr.Conditions {
		if c.Type != k.ReadyCondition {
			continue
		}
		if c.ObservedGeneration != 0 && c.ObservedGeneration < cr.Generation {
			return false
		}
		return c.Status == "True"
	}
	return false
}
