package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const GroupName = "exposure.socialweb.computer"

const Version = "v1alpha1"

var GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}
