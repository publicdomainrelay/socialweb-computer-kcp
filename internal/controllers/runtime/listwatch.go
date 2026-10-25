package runtime

import clientfeatures "k8s.io/client-go/features"

type listThenWatch struct {
	clientfeatures.Gates
}

func (g listThenWatch) Enabled(key clientfeatures.Feature) bool {
	return key != clientfeatures.WatchListClient && g.Gates.Enabled(key)
}

func init() {
	clientfeatures.ReplaceFeatureGates(listThenWatch{Gates: clientfeatures.FeatureGates()})
}
