//go:build integration

package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var stateSyncGVR = schema.GroupVersionResource{Group: "socialweb.computer", Version: "v1alpha1", Resource: "statesyncs"}

func TestOutOfBandComputeVMDeletionPublishesTermination(t *testing.T) {
	t.Parallel()
	n := StartNetwork(t, NetworkOptions{Bidders: 1})
	ctx := context.Background()
	name := "term" + randHex(3)
	marker := "termination-held-" + randHex(4)
	p := n.Start("run-request", restartRequestWait, "run", "request", "--network", n.Instance, "--name", name, "--hold",
		"--exec", "echo "+marker+"; read -r _", "--contract-timeout", "4m", "--network-timeout", "2m", "--state-dir", n.StateRoot)
	if err := p.WaitOutput(marker); err != nil {
		t.Fatalf("the guest did not run the command: %v", err)
	}
	vms, err := n.BidderVMsOf(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(vms) != 1 {
		t.Fatalf("want one ComputeVM on %s, found %d", n.bidderName(0), len(vms))
	}
	vm := vms[0]
	if vm.UID == "" {
		t.Fatal("the ComputeVM has no UID")
	}
	sum := sha256.Sum256([]byte(string(vm.UID)))
	syncName := "termination-" + hex.EncodeToString(sum[:8])
	t.Logf("deleting ComputeVM %s out of band; expecting StateSync %s to reach Acked", vm.Name, syncName)

	workspace := "root:" + n.bidderName(0)
	claimClient, err := n.bidderClient(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := claimClient.Resource(computeVMGVR).Delete(ctx, vm.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("deleting the ComputeVM out of band: %v", err)
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		if err := n.health(); err != nil {
			t.Fatal(err)
		}
		gone := false
		if _, err := claimClient.Resource(computeVMGVR).Get(ctx, vm.Name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			gone = true
		}
		if gone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the ComputeVM %s stayed after the out-of-band delete", vm.Name)
		}
		time.Sleep(500 * time.Millisecond)
	}
	ri := n.componentLinkClient(t, workspace).Resource(stateSyncGVR)
	deadline = time.Now().Add(2 * time.Minute)
	for {
		if err := n.health(); err != nil {
			t.Fatal(err)
		}
		u, err := ri.Get(ctx, syncName, metav1.GetOptions{})
		if err == nil {
			phase, _, _ := unstructuredString(u.Object, "status", "phase")
			ref, _, _ := unstructuredString(u.Object, "status", "acked", "ref")
			if phase == "Acked" && ref != "" {
				event, _, _ := unstructuredString(u.Object, "status", "acked", "eventUri")
				t.Logf("the out-of-band delete published one termination: payload %s event %s", ref, event)
				break
			}
		} else if !apierrors.IsNotFound(err) {
			t.Fatalf("reading the termination StateSync: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the termination StateSync %s did not reach Acked after the out-of-band delete (%v)", syncName, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = p.Input("done\n")
	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Log("the held request did not exit after the out-of-band delete; dev down will stop it")
	}
}

func unstructuredString(object map[string]any, fields ...string) (string, bool, error) {
	current := any(object)
	for _, field := range fields {
		m, ok := current.(map[string]any)
		if !ok {
			return "", false, fmt.Errorf("%s is not an object", field)
		}
		current, ok = m[field]
		if !ok {
			return "", false, nil
		}
	}
	value, ok := current.(string)
	return value, ok, nil
}
