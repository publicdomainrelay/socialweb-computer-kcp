package v1alpha1

import (
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type PrinterColumn = corev1alpha1.PrinterColumn

type APIResource = corev1alpha1.APIResource

var APIResources = []APIResource{
	{Kind: "Service", Plural: "services", Revision: 2, Export: "exposures", Object: Service{}, PrinterColumns: []PrinterColumn{
		{Name: "Protocol", Type: "string", JSONPath: ".spec.protocol"},
		{Name: "Target", Type: "string", JSONPath: ".spec.targetRef.name"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "Suspend", Type: "boolean", JSONPath: ".spec.suspend"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "Exposure", Plural: "exposures", Revision: 5, Export: "exposures", Object: Exposure{}, PrinterColumns: []PrinterColumn{
		{Name: "Mode", Type: "string", JSONPath: ".spec.mode"},
		{Name: "Hostname", Type: "string", JSONPath: ".spec.hostname"},
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "IngressRegistered", Type: "string", JSONPath: `.status.conditions[?(@.type=="IngressRegistered")].status`},
		{Name: "Address", Type: "string", JSONPath: ".status.address"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "ComponentLink", Plural: "componentlinks", Revision: 3, Export: "exposures", Object: ComponentLink{}, PrinterColumns: []PrinterColumn{
		{Name: "To Kind", Type: "string", JSONPath: ".spec.to.kind"},
		{Name: "To", Type: "string", JSONPath: ".spec.to.name"},
		{Name: "Suspend", Type: "boolean", JSONPath: ".spec.suspend"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}
