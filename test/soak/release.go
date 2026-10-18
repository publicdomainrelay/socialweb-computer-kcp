//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
)

func runRelease(args []string) int {
	fs := flag.NewFlagSet("release", flag.ExitOnError)
	instance := fs.String("instance", "soak", "market network instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/soak", "state dir of the instance")
	app := fs.String("app", "soakreq", "requester application name in the network")
	fs.Parse(args)
	ctx := context.Background()
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rec, err := t.childRecord(*instance + "-" + *app)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	dyn, err := clientOf(rec.AdminKubeconfig, workspaceOf(rec))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	contracts := dyn.Resource(appctl.ComputeContractGVR)
	list, err := contracts.List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, u := range list.Items {
		p := phaseOf(&u)
		if !strings.HasPrefix(u.GetName(), "vm-") || p == "Released" || p == "Failed" {
			continue
		}
		_, err := contracts.Patch(ctx, u.GetName(), types.MergePatchType, []byte(`{"spec":{"release":true}}`), metav1.PatchOptions{})
		fmt.Println(u.GetName(), p, err)
	}
	return 0
}
