package bidder

import (
	"encoding/json"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}

func jsonUnmarshal(raw []byte, out any) error {
	return json.Unmarshal(raw, out)
}

func isAlreadyExists(err error) bool {
	return apierrors.IsAlreadyExists(err)
}
