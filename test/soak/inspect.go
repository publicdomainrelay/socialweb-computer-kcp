//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	instance := fs.String("instance", "soak", "market network instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/soak", "state dir of the instance")
	app := fs.String("app", "soak-bidder-1", "child instance")
	resource := fs.String("resource", "computevms", "resource of group socialweb.computer")
	group := fs.String("group", "socialweb.computer", "API group")
	path := fs.String("path", "root", "logical cluster path")
	name := fs.String("name", "", "print this object whole")
	onlyUnowned := fs.Bool("unowned", true, "list only objects with no ownerReferences")
	fs.Parse(args)
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rec, err := t.childRecord(*app)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dyn, err := pathClient(rec.AdminKubeconfig, *path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	list, err := dyn.Resource(schema.GroupVersionResource{Group: *group, Version: "v1alpha1", Resource: *resource}).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, u := range list.Items {
		if *name != "" {
			if u.GetName() == *name {
				b, _ := json.MarshalIndent(u.Object, "", " ")
				fmt.Println(string(b))
			}
			continue
		}
		if *onlyUnowned && len(u.GetOwnerReferences()) > 0 {
			continue
		}
		phase, _, _ := unstructured.NestedString(u.Object, "status", "phase")
		b, _ := json.Marshal(map[string]any{"name": u.GetName(), "phase": phase, "ageSec": int(time.Since(u.GetCreationTimestamp().Time).Seconds()),
			"owners": len(u.GetOwnerReferences()), "finalizers": u.GetFinalizers(), "deleting": u.GetDeletionTimestamp() != nil, "annotations": u.GetAnnotations()})
		fmt.Println(string(b))
	}
	return 0
}
