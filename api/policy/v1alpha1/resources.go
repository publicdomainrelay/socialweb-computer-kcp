package v1alpha1

import (
	corev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

type PrinterColumn = corev1alpha1.PrinterColumn

type APIResource = corev1alpha1.APIResource

var APIResources = []APIResource{
	{Kind: "PolicyEngine", Plural: "policyengines", Revision: 1, Export: "policy", Object: PolicyEngine{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Ready", Type: "boolean", JSONPath: ".status.ready"},
		{Name: "Active", Type: "integer", JSONPath: ".status.activeRuns"},
		{Name: "Suspend", Type: "boolean", JSONPath: ".spec.suspend"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PolicyWorkflow", Plural: "policyworkflows", Revision: 2, Export: "policy", Object: PolicyWorkflow{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Engine", Type: "string", JSONPath: ".spec.engineRef"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PolicyWorkflowRun", Plural: "policyworkflowruns", Revision: 1, Export: "policy", Object: PolicyWorkflowRun{}, PrinterColumns: []PrinterColumn{
		{Name: "Phase", Type: "string", JSONPath: ".status.phase"},
		{Name: "Engine", Type: "string", JSONPath: ".spec.engineRef"},
		{Name: "Exit", Type: "string", JSONPath: ".status.exitStatus"},
		{Name: "Restarts", Type: "integer", JSONPath: ".status.restarts"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
	{Kind: "PolicyTrigger", Plural: "policytriggers", Revision: 2, Export: "policy", Object: PolicyTrigger{}, PrinterColumns: []PrinterColumn{
		{Name: "Source", Type: "string", JSONPath: ".spec.source.type"},
		{Name: "LastRun", Type: "date", JSONPath: ".status.lastRun"},
		{Name: "NextRun", Type: "date", JSONPath: ".status.nextRun"},
		{Name: "Active", Type: "integer", JSONPath: ".status.active"},
		{Name: "Suspend", Type: "boolean", JSONPath: ".spec.suspend"},
		{Name: "Age", Type: "date", JSONPath: ".metadata.creationTimestamp"},
	}},
}
