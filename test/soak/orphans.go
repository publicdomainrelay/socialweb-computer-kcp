//go:build linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/appctl"
)

type listedRecord struct {
	URI   string         `json:"uri"`
	Value map[string]any `json:"value"`
}

func listCollection(client *http.Client, base, did, collection string) ([]listedRecord, error) {
	var out []listedRecord
	cursor := ""
	for {
		path := base + "/xrpc/com.atproto.repo.listRecords?limit=100&repo=" + did + "&collection=" + collection
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		resp, err := client.Get(path)
		if err != nil {
			return out, err
		}
		var page struct {
			Records []listedRecord `json:"records"`
			Cursor  string         `json:"cursor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return out, err
		}
		out = append(out, page.Records...)
		if page.Cursor == "" || len(page.Records) == 0 {
			return out, nil
		}
		cursor = page.Cursor
	}
}

func refURI(v any) string {
	m, _ := v.(map[string]any)
	s, _ := m["uri"].(string)
	return s
}

func runOrphans(args []string) int {
	fs := flag.NewFlagSet("orphans", flag.ExitOnError)
	instance := fs.String("instance", "soak", "market network instance")
	stateDir := fs.String("state-dir", "/var/tmp/swcit/soak", "state dir of the instance")
	binary := fs.String("binary", "/var/tmp/soak/bin/socialweb-computer", "socialweb-computer binary")
	app := fs.String("app", "soakreq", "requester application name in the network")
	olderThan := fs.Duration("older-than", time.Hour, "count records older than this")
	fs.Parse(args)
	ctx := context.Background()
	t, err := openTarget(*instance, *stateDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	raw, err := exec.CommandContext(ctx, *binary, "dev", "status", "--instance", *instance, "--state-dir", *stateDir).Output()
	var nv networkView
	if err != nil || json.Unmarshal(raw, &nv) != nil {
		fmt.Fprintln(os.Stderr, "dev status:", err)
		return 1
	}
	base, did := "", ""
	for _, a := range nv.Network.Status.Apps {
		if a.Name != *app {
			continue
		}
		did = a.Did
		for _, l := range a.Listeners {
			if l.Name == "tcp" && (l.Component == "pds" || (l.Component == "requester" && base == "")) {
				base = "http://" + l.Address
			}
		}
	}
	if base == "" {
		fmt.Fprintln(os.Stderr, "no repo listener for", *app)
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
	list, err := dyn.Resource(appctl.ComputeContractGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	claimed := map[string]string{}
	for _, u := range list.Items {
		for _, f := range []string{"payload", "rfp"} {
			if s, _, _ := unstructured.NestedString(u.Object, "status", "contract", f, "uri"); s != "" {
				claimed[s] = u.GetName()
			}
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	vms, err := listCollection(client, base, did, "com.publicdomainrelay.temp.compute.vm")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rfps, err := listCollection(client, base, did, "com.publicdomainrelay.temp.market.rfp")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	rfpOf := map[string]string{}
	for _, r := range rfps {
		rfpOf[refURI(r.Value["payload"])] = r.URI
	}
	old, orphans := 0, 0
	for _, v := range vms {
		created, _ := time.Parse(time.RFC3339, fmt.Sprint(v.Value["createdAt"]))
		if time.Since(created) < *olderThan {
			continue
		}
		old++
		if claimed[v.URI] != "" || claimed[rfpOf[v.URI]] != "" {
			continue
		}
		orphans++
		b, _ := json.Marshal(map[string]any{"vm": v.URI, "rfp": rfpOf[v.URI], "role": v.Value["role"], "createdAt": v.Value["createdAt"]})
		fmt.Println(string(b))
	}
	b, _ := json.Marshal(map[string]any{"contracts": len(list.Items), "vms": len(vms), "rfps": len(rfps), "older": old, "unclaimed": orphans,
		"olderThan": olderThan.String(), "repo": strings.TrimPrefix(base, "http://")})
	fmt.Println(string(b))
	return 0
}
