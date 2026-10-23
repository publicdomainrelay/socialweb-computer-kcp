package kcpgvr

import "k8s.io/apimachinery/pkg/runtime/schema"

var (
	Workspaces         = schema.GroupVersionResource{Group: "tenancy.kcp.io", Version: "v1alpha1", Resource: "workspaces"}
	APIBindings        = schema.GroupVersionResource{Group: "apis.kcp.io", Version: "v1alpha2", Resource: "apibindings"}
	APIExports         = schema.GroupVersionResource{Group: "apis.kcp.io", Version: "v1alpha2", Resource: "apiexports"}
	APIResourceSchemas = schema.GroupVersionResource{Group: "apis.kcp.io", Version: "v1alpha1", Resource: "apiresourceschemas"}
	LogicalClusters    = schema.GroupVersionResource{Group: "core.kcp.io", Version: "v1alpha1", Resource: "logicalclusters"}

	ConfigMaps      = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	Secrets         = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	ServiceAccounts = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
	Services        = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	Events          = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	ResourceQuotas  = schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}
	Namespaces      = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	Pods            = schema.GroupVersionResource{Version: "v1", Resource: "pods"}

	Leases             = schema.GroupVersionResource{Group: "coordination.k8s.io", Version: "v1", Resource: "leases"}
	SelfSubjectReviews = schema.GroupVersionResource{Group: "authentication.k8s.io", Version: "v1", Resource: "selfsubjectreviews"}

	ClusterRoles        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	ClusterRoleBindings = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	Roles               = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}
	RoleBindings        = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}

	Deployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	DaemonSets  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}

	CustomResourceDefinitions         = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	ValidatingWebhookConfigurations   = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingwebhookconfigurations"}
	ValidatingAdmissionPolicies       = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicies"}
	ValidatingAdmissionPolicyBindings = schema.GroupVersionResource{Group: "admissionregistration.k8s.io", Version: "v1", Resource: "validatingadmissionpolicybindings"}

	ClusterSecretStores = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "clustersecretstores"}
	SecretStores        = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "secretstores"}
	ExternalSecrets     = schema.GroupVersionResource{Group: "external-secrets.io", Version: "v1", Resource: "externalsecrets"}
)
