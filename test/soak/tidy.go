//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
)

func runTidy(args []string) int {
	fs := flag.NewFlagSet("tidy", flag.ExitOnError)
	instance := fs.String("instance", "soak", "market network instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/soak", "state dir of the instance")
	interval := fs.String("interval", "15m", "tidy interval to set on every OpenBaoServer")
	fs.Parse(args)
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	kubeconfigs := map[string][2]string{"network": {t.record.AdminKubeconfig, workspaceOf(t.record)}}
	apps, _ := filepath.Glob(filepath.Join(t.layout.StateDir, appctl.AppsDir, "*", "*", "instance.json"))
	for _, a := range apps {
		dir := filepath.Base(filepath.Dir(a))
		rec, err := t.childRecord(dir)
		if err == nil {
			kubeconfigs[dir] = [2]string{rec.AdminKubeconfig, workspaceOf(rec)}
		}
	}
	for name, kc := range kubeconfigs {
		dyn, err := clientOf(kc[0], kc[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, name, err)
			continue
		}
		ri := dyn.Resource(appctl.OpenBaoServerGVR)
		list, err := ri.List(context.Background(), metav1.ListOptions{})
		if err != nil {
			fmt.Fprintln(os.Stderr, name, err)
			continue
		}
		for _, u := range list.Items {
			spec := map[string]any{"tidy": map[string]any{"interval": *interval}}
			if tidies, found, _ := unstructured.NestedSlice(u.Object, "spec", "declared", "pkiTidy"); found {
				for i := range tidies {
					if m, ok := tidies[i].(map[string]any); ok {
						m["interval"] = *interval
					}
				}
				spec["declared"] = map[string]any{"pkiTidy": tidies}
			}
			body, _ := json.Marshal(map[string]any{"spec": spec})
			_, err := ri.Patch(context.Background(), u.GetName(), types.MergePatchType, body, metav1.PatchOptions{FieldManager: "soak-tidy"})
			fmt.Printf("%s/%s: pkiTidy=%v patch err=%v\n", name, u.GetName(), len(spec), err)
		}
	}
	return 0
}
