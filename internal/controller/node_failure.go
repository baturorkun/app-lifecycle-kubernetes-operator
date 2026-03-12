/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
)

// findFullyLocalWorkloads returns Deployments and StatefulSets in the policy's targetNamespace
// whose ALL pods are running on the given failedNodeName. Workloads that have even one pod on a
// different (healthy) node are excluded, as they can continue serving traffic.
func (r *NamespaceLifecyclePolicyReconciler) findFullyLocalWorkloads(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	failedNodeName string,
) ([]appsv1.Deployment, []appsv1.StatefulSet, error) {
	log := logf.FromContext(ctx)

	// Build the set of selector-matching workload UIDs first.
	// The policy selector filters Deployments/StatefulSets by their own metadata labels —
	// NOT pod labels. Pod labels come from the pod template and differ from the workload's
	// metadata labels, so we must NOT apply the selector when listing pods.
	allowedDeployUIDs := map[types.UID]appsv1.Deployment{}
	allowedSTSUIDs := map[types.UID]appsv1.StatefulSet{}

	allowedDeploys, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list deployments: %w", err)
	}
	for _, d := range allowedDeploys.Items {
		allowedDeployUIDs[d.UID] = d
	}

	allowedSTS, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list statefulsets: %w", err)
	}
	for _, s := range allowedSTS.Items {
		allowedSTSUIDs[s.UID] = s
	}

	// List ALL pods in the namespace — no label selector, because pod labels are from the
	// pod template and are independent of the policy selector which targets workload metadata.
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(policy.Spec.TargetNamespace)); err != nil {
		return nil, nil, fmt.Errorf("failed to list pods in namespace %s: %w", policy.Spec.TargetNamespace, err)
	}

	// workloadEntry holds per-workload pod counters
	type workloadEntry struct {
		kind           string // "Deployment" or "StatefulSet"
		uid            types.UID
		totalPods      int
		failedNodePods int
	}

	// key: workload UID → entry
	workloads := make(map[types.UID]*workloadEntry)

	for i := range podList.Items {
		pod := &podList.Items[i]

		// Skip completed pods — they are no longer running so they don't indicate live traffic
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// Skip Terminating pods — they are being garbage-collected and no longer serving traffic.
		// Counting them would cause "fully local" checks to return false positives when eviction
		// races with rescheduling (pod on failed node is Terminating while replacement is Pending).
		if pod.DeletionTimestamp != nil {
			continue
		}

		ownerKind, ownerUID, err := r.resolveWorkloadOwnerUID(ctx, pod)
		if err != nil {
			log.V(1).Info("Could not resolve workload owner for pod, skipping",
				"pod", pod.Name, "namespace", pod.Namespace, "error", err)
			continue
		}
		if ownerKind == "" {
			// Unmanaged standalone pod — not part of a Deployment/StatefulSet
			continue
		}

		// Only track pods whose owning workload matches the policy selector
		switch ownerKind {
		case "Deployment":
			if _, ok := allowedDeployUIDs[ownerUID]; !ok {
				continue
			}
		case "StatefulSet":
			if _, ok := allowedSTSUIDs[ownerUID]; !ok {
				continue
			}
		}

		if _, exists := workloads[ownerUID]; !exists {
			workloads[ownerUID] = &workloadEntry{kind: ownerKind, uid: ownerUID}
		}
		workloads[ownerUID].totalPods++
		if pod.Spec.NodeName == failedNodeName {
			workloads[ownerUID].failedNodePods++
		}
	}

	var deployments []appsv1.Deployment
	var statefulsets []appsv1.StatefulSet

	for uid, entry := range workloads {
		if entry.totalPods == 0 {
			continue
		}
		// Only include workloads where EVERY pod is on the failed node
		if entry.totalPods != entry.failedNodePods {
			log.V(1).Info("Skipping workload: has pods on other nodes",
				"kind", entry.kind, "uid", uid,
				"totalPods", entry.totalPods, "failedNodePods", entry.failedNodePods)
			continue
		}

		switch entry.kind {
		case "Deployment":
			if d, ok := allowedDeployUIDs[uid]; ok {
				deployments = append(deployments, d)
			}
		case "StatefulSet":
			if s, ok := allowedSTSUIDs[uid]; ok {
				statefulsets = append(statefulsets, s)
			}
		}
	}

	return deployments, statefulsets, nil
}

// resolveWorkloadOwnerUID traces a pod's OwnerReferences to find the managing Deployment or
// StatefulSet UID. For pods owned by a ReplicaSet the RS's own OwnerReferences are checked to
// find a parent Deployment. Returns ("", "", nil) for unmanaged pods.
func (r *NamespaceLifecyclePolicyReconciler) resolveWorkloadOwnerUID(
	ctx context.Context,
	pod *corev1.Pod,
) (kind string, uid types.UID, err error) {
	for _, ref := range pod.OwnerReferences {
		switch ref.Kind {
		case "ReplicaSet":
			rs := &appsv1.ReplicaSet{}
			if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: pod.Namespace}, rs); err != nil {
				return "", "", fmt.Errorf("failed to get ReplicaSet %s: %w", ref.Name, err)
			}
			for _, rsRef := range rs.OwnerReferences {
				if rsRef.Kind == "Deployment" {
					return "Deployment", rsRef.UID, nil
				}
			}
			// Standalone ReplicaSet (no Deployment parent) — not managed by a Deployment
			return "", "", nil

		case "StatefulSet":
			return "StatefulSet", ref.UID, nil
		}
	}
	return "", "", nil
}

// forceDeleteTerminatingPods force-deletes (grace period 0) all pods in the policy's
// targetNamespace that are stuck in Terminating state on the given failedNode. This cleans
// up ghost pods that will never be acknowledged by the dead kubelet.
func (r *NamespaceLifecyclePolicyReconciler) forceDeleteTerminatingPods(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	failedNode string,
) {
	log := logf.FromContext(ctx)

	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(policy.Spec.TargetNamespace)); err != nil {
		log.Error(err, "Failed to list pods for force-delete", "namespace", policy.Spec.TargetNamespace)
		return
	}

	deleted := 0
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName != failedNode || pod.DeletionTimestamp == nil {
			continue
		}
		gracePeriod := int64(0)
		if err := r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &gracePeriod}); err != nil {
			log.Error(err, "Failed to force-delete terminating pod",
				"pod", pod.Name, "namespace", pod.Namespace, "node", failedNode)
			continue
		}
		log.Info("🗑️ Force-deleted terminating pod on failed node",
			"policy", policy.Name, "pod", pod.Name, "namespace", pod.Namespace, "node", failedNode)
		deleted++
	}

	if deleted > 0 {
		log.Info("🗑️ Force-deleted terminating pods on failed node",
			"policy", policy.Name, "count", deleted, "node", failedNode)
	}
}

// handleNodeFailureEvent scales down all Deployments and StatefulSets in the policy's
// targetNamespace whose ALL pods are on the failed node. Returns the list of workload
// names that were scaled to 0.
func (r *NamespaceLifecyclePolicyReconciler) handleNodeFailureEvent(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
) ([]string, error) {
	log := logf.FromContext(ctx)

	// Always force-delete terminating pods on the failed node first — they are guaranteed
	// dead and the kubelet will never acknowledge their deletion.
	r.forceDeleteTerminatingPods(ctx, policy, policy.Status.FailedNodeName)

	// If the namespace is already fully frozen, workloads are already at 0 — nothing else to do
	if policy.Status.Phase == appsv1alpha1.PhaseFrozen {
		log.Info("🧊 Node failure detected but namespace is already Frozen — skipping scale-down",
			"policy", policy.Name,
			"failedNode", policy.Status.FailedNodeName)
		return nil, nil
	}

	deployments, statefulsets, err := r.findFullyLocalWorkloads(ctx, policy, policy.Status.FailedNodeName)
	if err != nil {
		return nil, err
	}

	var affectedWorkloads []string

	for i := range deployments {
		deploy := &deployments[i]
		log.Info("⚠️ Node failure: scaling down Deployment (all pods on failed node)",
			"deployment", deploy.Name,
			"namespace", deploy.Namespace,
			"failedNode", policy.Status.FailedNodeName)
		if err := r.freezeDeployment(ctx, deploy, nil); err != nil {
			log.Error(err, "Failed to scale down Deployment during node failure", "deployment", deploy.Name)
			continue
		}
		affectedWorkloads = append(affectedWorkloads, "deploy/"+deploy.Name)
	}

	for i := range statefulsets {
		sts := &statefulsets[i]
		log.Info("⚠️ Node failure: scaling down StatefulSet (all pods on failed node)",
			"statefulset", sts.Name,
			"namespace", sts.Namespace,
			"failedNode", policy.Status.FailedNodeName)
		if err := r.freezeStatefulSet(ctx, sts, nil); err != nil {
			log.Error(err, "Failed to scale down StatefulSet during node failure", "statefulset", sts.Name)
			continue
		}
		affectedWorkloads = append(affectedWorkloads, "sts/"+sts.Name)
	}

	return affectedWorkloads, nil
}

// setDegradedCondition sets or updates the "Degraded" condition on the policy's Conditions slice.
func setDegradedCondition(conditions *[]metav1.Condition, failedNode string, affectedWorkloads []string) {
	msg := fmt.Sprintf("Node %s failed; scaled down workloads: %s",
		failedNode, strings.Join(affectedWorkloads, ", "))
	if len(affectedWorkloads) == 0 {
		msg = fmt.Sprintf("Node %s failed; no workloads were fully local to this node", failedNode)
	}

	now := metav1.Now()
	newCond := metav1.Condition{
		Type:               "Degraded",
		Status:             metav1.ConditionTrue,
		Reason:             "NodeFailure",
		Message:            msg,
		LastTransitionTime: now,
	}

	for i, c := range *conditions {
		if c.Type == "Degraded" {
			(*conditions)[i] = newCond
			return
		}
	}
	*conditions = append(*conditions, newCond)
}

// HandleNodeFailureAtStartup is called from the startup goroutine when a NotReady node is detected
// before the startup resume phase. It synchronously scales down all fully-local workloads for the
// given policy and updates the policy status so that PHASE 2 of startup can resume surviving
// workloads on healthy nodes.
func (r *NamespaceLifecyclePolicyReconciler) HandleNodeFailureAtStartup(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	failedNode string,
) error {
	log := logf.FromContext(ctx)

	// Ensure FailedNodeName is set before calling handleNodeFailureEvent (it reads from status)
	policy.Status.FailedNodeName = failedNode
	now := metav1.Now()
	policy.Status.NodeFailureEventDetectedAt = &now

	log.Info("🔴 Startup pre-scan: scaling down fully-local workloads before resume",
		"policy", policy.Name,
		"failedNode", failedNode)

	affectedWorkloads, err := r.handleNodeFailureEvent(ctx, policy)
	if err != nil {
		log.Error(err, "Startup pre-scan: failed to handle node failure", "policy", policy.Name)
		return err
	}

	// Fetch latest to avoid conflict on status update
	latest := policy.DeepCopy()
	if fetchErr := r.Get(ctx, client.ObjectKeyFromObject(policy), latest); fetchErr != nil {
		log.Error(fetchErr, "Startup pre-scan: failed to re-fetch policy for status update", "policy", policy.Name)
		return fetchErr
	}

	handledAt := metav1.Now()
	latest.Status.FailedNodeName = failedNode
	latest.Status.NodeFailureEventDetectedAt = &now
	latest.Status.NodeFailureEventHandledAt = &handledAt
	latest.Status.AffectedWorkloads = affectedWorkloads
	// PendingStartupResume is intentionally left false here — startup PHASE 2 will
	// run the resume directly via processPolicyWithDelayAndResume.
	setDegradedCondition(&latest.Status.Conditions, failedNode, affectedWorkloads)

	if len(affectedWorkloads) > 0 {
		log.Info("🔴 Startup pre-scan: namespace DEGRADED due to node failure",
			"policy", policy.Name,
			"failedNode", failedNode,
			"affectedWorkloads", affectedWorkloads)
	} else {
		log.Info("✅ Startup pre-scan: no fully-local workloads found — node failure handled",
			"policy", policy.Name,
			"failedNode", failedNode)
	}

	// Set PendingStartupResume=true so the reconcile loop re-resumes workloads
	// on the surviving nodes once scale-down is complete.
	latest.Status.PendingStartupResume = true
	latest.Status.ResumeDelayStartedAt = &handledAt

	if updateErr := r.Status().Update(ctx, latest); updateErr != nil {
		log.Error(updateErr, "Startup pre-scan: failed to update policy status after node failure", "policy", policy.Name)
		return updateErr
	}

	log.Info("🔄 Startup pre-scan: node failure handled — reconcile loop will resume on surviving nodes",
		"policy", policy.Name,
		"failedNode", failedNode)

	// Propagate the updated status back to the caller's copy
	policy.Status = latest.Status
	return nil
}

// clearDegradedCondition removes the "Degraded" condition from the policy's Conditions slice.
func clearDegradedCondition(conditions *[]metav1.Condition) {
	filtered := (*conditions)[:0]
	for _, c := range *conditions {
		if c.Type != "Degraded" {
			filtered = append(filtered, c)
		}
	}
	*conditions = filtered
}
