package v1alpha1

import (
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type PrinterColumn = corev1alpha1.PrinterColumn

type APIResource = corev1alpha1.APIResource

var APIResources = []APIResource{
	{Kind: "RemotePDS", Plural: "remotepds", Revision: 1, Export: "pds", Object: RemotePDS{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "Endpoint", Type: "string", JSONPath: ".status.endpoint"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}
