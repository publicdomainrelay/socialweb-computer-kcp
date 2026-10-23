package leasecontroller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
)

func (c *Controller) ReconcileClusterRoll(ctx context.Context, workspace string, u *unstructured.Unstructured) error {
	if u == nil {
		return errors.New("leasecontroller: a ClusterRoll object is required")
	}
	dyn, err := c.dynamic(workspace)
	if err != nil {
		return fmt.Errorf("leasecontroller: opening the dynamic client for workspace %q: %w", workspace, err)
	}
	var roll v1alpha1.ClusterRoll
	if err := decodeObject(u, &roll); err != nil {
		return fmt.Errorf("leasecontroller: decoding ClusterRoll %s: %w", u.GetName(), err)
	}

	var before v1alpha1.ClusterRollStatus
	roll.Status.DeepCopyInto(&before)
	var status v1alpha1.ClusterRollStatus
	roll.Status.DeepCopyInto(&status)
	generation := u.GetGeneration()
	status.ObservedGeneration = generation

	cluster := strings.TrimSpace(roll.Spec.ClusterRef.Name)
	if cluster == "" {
		status.Phase = v1alpha1.ClusterRollPhasePending
		status.StartedAt = ""
		status.CompletedAt = ""
		message := "the ClusterRoll names no cluster"
		setReady(&status.Conditions, generation, false, "InvalidSpec", message)
		setCondition(&status.Conditions, generation, ConditionRolling, metav1.ConditionFalse, "InvalidSpec", message)
		return writeStatus(ctx, dyn, ClusterRollGVR, u, before, status)
	}

	slots, rollErr := c.rolls(cluster)
	if rollErr != nil {
		status.Phase = v1alpha1.ClusterRollPhasePending
		status.StartedAt = ""
		status.CompletedAt = ""
		setReady(&status.Conditions, generation, false, "RollStateUnavailable", rollErr.Error())
		setCondition(&status.Conditions, generation, ConditionRolling, metav1.ConditionFalse, "RollStateUnavailable", rollErr.Error())
		if err := writeStatus(ctx, dyn, ClusterRollGVR, u, before, status); err != nil {
			return err
		}
		return fmt.Errorf("leasecontroller: reading the kcpserver roll state for cluster %s: %w", cluster, rollErr)
	}

	active := strings.TrimSpace(slots.Active)
	if active == "" {
		status.Phase = v1alpha1.ClusterRollPhasePending
		status.StartedAt = ""
		status.CompletedAt = ""
		message := fmt.Sprintf("the kcpserver records no active slot for cluster %s", cluster)
		setReady(&status.Conditions, generation, false, "AwaitingRoll", message)
		setCondition(&status.Conditions, generation, ConditionRolling, metav1.ConditionFalse, "AwaitingRoll", message)
		return writeStatus(ctx, dyn, ClusterRollGVR, u, before, status)
	}

	record, ok := slots.Slots[active]
	if !ok {
		status.Phase = v1alpha1.ClusterRollPhaseFailed
		status.StartedAt = ""
		status.CompletedAt = ""
		message := fmt.Sprintf("the kcpserver records active slot %q for cluster %s but no record for it", active, cluster)
		setReady(&status.Conditions, generation, false, "RollStateInconsistent", message)
		setCondition(&status.Conditions, generation, ConditionRolling, metav1.ConditionFalse, "RollStateInconsistent", message)
		return writeStatus(ctx, dyn, ClusterRollGVR, u, before, status)
	}

	started := record.StartedAt
	if started.IsZero() {
		started = slots.Updated
	}
	status.StartedAt = formatTime(started)

	if record.ReadyAt.IsZero() {
		status.Phase = v1alpha1.ClusterRollPhaseRunning
		status.CompletedAt = ""
		message := fmt.Sprintf("the kcpserver is bringing slot %s of cluster %s up on port %d", active, cluster, record.Port)
		setReady(&status.Conditions, generation, false, "RollInProgress", message)
		setCondition(&status.Conditions, generation, ConditionRolling, metav1.ConditionTrue, "RollInProgress", message)
		return writeStatus(ctx, dyn, ClusterRollGVR, u, before, status)
	}

	status.Phase = v1alpha1.ClusterRollPhaseCompleted
	status.CompletedAt = formatTime(record.ReadyAt)
	message := fmt.Sprintf("the kcpserver records slot %s of cluster %s ready on port %d", active, cluster, record.Port)
	setReady(&status.Conditions, generation, true, "RollCompleted", message)
	setCondition(&status.Conditions, generation, ConditionRolling, metav1.ConditionFalse, "RollCompleted", message)
	return writeStatus(ctx, dyn, ClusterRollGVR, u, before, status)
}
