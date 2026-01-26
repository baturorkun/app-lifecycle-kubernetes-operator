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
	"net/http"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
)

// NamespaceLifecyclePolicyReconciler reconciles a NamespaceLifecyclePolicy object
type NamespaceLifecyclePolicyReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RESTClient rest.Interface
}

const (
	// nilAnnotationValue is used when original terminationGracePeriodSeconds is nil
	nilAnnotationValue = "nil"
)

// +kubebuilder:rbac:groups=apps.ops.dev,resources=namespacelifecyclepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.ops.dev,resources=namespacelifecyclepolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.ops.dev,resources=namespacelifecyclepolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes/proxy,verbs=get

// shouldSkipOperation checks if the operation should be skipped based on operationId
func (r *NamespaceLifecyclePolicyReconciler) shouldSkipOperation(policy *appsv1alpha1.NamespaceLifecyclePolicy) bool {
	// If no operationId specified, always process
	if policy.Spec.OperationId == "" {
		return false
	}

	// Don't skip if pre-conditions are still being checked
	// This allows the reconcile loop to continue checking pre-conditions
	if policy.Status.PreConditionsStatus != nil && policy.Status.PreConditionsStatus.Checking {
		return false
	}

	// Don't skip if pending startup resume (non-blocking pre-conditions)
	if policy.Status.PendingStartupResume {
		return false
	}

	// Check if this operationId was already handled
	return policy.Status.LastHandledOperationId == policy.Spec.OperationId
}

// listDeployments lists deployments in the target namespace, optionally filtered by label selector
func (r *NamespaceLifecyclePolicyReconciler) listDeployments(ctx context.Context, namespace string, selector *metav1.LabelSelector) (*appsv1.DeploymentList, error) {
	deploymentList := &appsv1.DeploymentList{}
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
	}

	// Add label selector if specified
	if selector != nil {
		labelSelector, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, err
		}
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: labelSelector})
	}

	if err := r.List(ctx, deploymentList, listOpts...); err != nil {
		return nil, err
	}

	return deploymentList, nil
}

// listStatefulSets lists statefulsets in the target namespace, optionally filtered by label selector
func (r *NamespaceLifecyclePolicyReconciler) listStatefulSets(ctx context.Context, namespace string, selector *metav1.LabelSelector) (*appsv1.StatefulSetList, error) {
	statefulSetList := &appsv1.StatefulSetList{}
	listOpts := []client.ListOption{
		client.InNamespace(namespace),
	}

	// Add label selector if specified
	if selector != nil {
		labelSelector, err := metav1.LabelSelectorAsSelector(selector)
		if err != nil {
			return nil, err
		}
		listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: labelSelector})
	}

	if err := r.List(ctx, statefulSetList, listOpts...); err != nil {
		return nil, err
	}

	return statefulSetList, nil
}

// freezeDeployment sets the deployment replicas to 0 and stores the original count in an annotation
func (r *NamespaceLifecyclePolicyReconciler) freezeDeployment(ctx context.Context, deployment *appsv1.Deployment, policy *appsv1alpha1.NamespaceLifecyclePolicy) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestDeployment := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, latestDeployment); err != nil {
			return err
		}

		// If already frozen (replicas = 0), skip
		if latestDeployment.Spec.Replicas != nil && *latestDeployment.Spec.Replicas == 0 {
			return nil
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestDeployment.DeepCopy()

		// Store original replica count in annotation
		if latestDeployment.Annotations == nil {
			latestDeployment.Annotations = make(map[string]string)
		}

		originalReplicas := int32(1) // default
		if latestDeployment.Spec.Replicas != nil {
			originalReplicas = *latestDeployment.Spec.Replicas
		}

		// Store original replicas and optional properties
		latestDeployment.Annotations[appsv1alpha1.AnnotationOriginalReplicas] = strconv.Itoa(int(originalReplicas))

		// Handle terminationGracePeriodSeconds override
		if policy != nil && policy.Spec.TerminationGracePeriodSeconds != nil && policy.Spec.TerminationGracePeriodSeconds.Deployment != nil {
			if latestDeployment.Spec.Template.Spec.TerminationGracePeriodSeconds != nil {
				latestDeployment.Annotations[appsv1alpha1.AnnotationOriginalTerminationGracePeriod] = strconv.FormatInt(*latestDeployment.Spec.Template.Spec.TerminationGracePeriodSeconds, 10)
			} else {
				latestDeployment.Annotations[appsv1alpha1.AnnotationOriginalTerminationGracePeriod] = nilAnnotationValue
			}
			latestDeployment.Spec.Template.Spec.TerminationGracePeriodSeconds = policy.Spec.TerminationGracePeriodSeconds.Deployment
		}

		// Set replicas to 0
		zero := int32(0)
		latestDeployment.Spec.Replicas = &zero

		// Use Patch with MergeFrom to be resilient to concurrent status updates
		return r.Patch(ctx, latestDeployment, client.MergeFrom(patchBase))
	})
}

// freezeStatefulSet sets the statefulset replicas to 0 and stores the original count in an annotation
func (r *NamespaceLifecyclePolicyReconciler) freezeStatefulSet(ctx context.Context, sts *appsv1.StatefulSet, policy *appsv1alpha1.NamespaceLifecyclePolicy) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestSts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, latestSts); err != nil {
			return err
		}

		// If already frozen (replicas = 0), skip
		if latestSts.Spec.Replicas != nil && *latestSts.Spec.Replicas == 0 {
			return nil
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestSts.DeepCopy()

		// Store original replica count in annotation
		if latestSts.Annotations == nil {
			latestSts.Annotations = make(map[string]string)
		}

		originalReplicas := int32(1) // default
		if latestSts.Spec.Replicas != nil {
			originalReplicas = *latestSts.Spec.Replicas
		}

		// Store original replicas and optional properties
		latestSts.Annotations[appsv1alpha1.AnnotationOriginalReplicas] = strconv.Itoa(int(originalReplicas))

		// Handle terminationGracePeriodSeconds override
		if policy != nil && policy.Spec.TerminationGracePeriodSeconds != nil && policy.Spec.TerminationGracePeriodSeconds.StatefulSet != nil {
			if latestSts.Spec.Template.Spec.TerminationGracePeriodSeconds != nil {
				latestSts.Annotations[appsv1alpha1.AnnotationOriginalTerminationGracePeriod] = strconv.FormatInt(*latestSts.Spec.Template.Spec.TerminationGracePeriodSeconds, 10)
			} else {
				latestSts.Annotations[appsv1alpha1.AnnotationOriginalTerminationGracePeriod] = nilAnnotationValue
			}
			latestSts.Spec.Template.Spec.TerminationGracePeriodSeconds = policy.Spec.TerminationGracePeriodSeconds.StatefulSet
		}

		// Set replicas to 0
		zero := int32(0)
		latestSts.Spec.Replicas = &zero

		// Use Patch with MergeFrom to be resilient to concurrent status updates
		return r.Patch(ctx, latestSts, client.MergeFrom(patchBase))
	})
}

// resumeDeployment restores the deployment replicas from the annotation
func (r *NamespaceLifecyclePolicyReconciler) resumeDeployment(ctx context.Context, deployment *appsv1.Deployment) error {
	log := ctrl.Log

	// Check if there's a stored original replica count
	originalReplicasStr, exists := deployment.Annotations[appsv1alpha1.AnnotationOriginalReplicas]
	if !exists {
		log.V(1).Info("Skipping resume: deployment was not frozen",
			"deployment", deployment.Name,
			"namespace", deployment.Namespace,
			"reason", "Deployment has not been frozen by this operator")
		return nil
	}

	originalReplicas, err := strconv.Atoi(originalReplicasStr)
	if err != nil {
		return err
	}

	// Use retry to handle concurrent updates
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestDeployment := &appsv1.Deployment{}
		if err := r.Get(ctx, types.NamespacedName{Name: deployment.Name, Namespace: deployment.Namespace}, latestDeployment); err != nil {
			return err
		}

		// Check again if annotation still exists (might have been resumed already)
		if _, exists := latestDeployment.Annotations[appsv1alpha1.AnnotationOriginalReplicas]; !exists {
			log.V(1).Info("Deployment already resumed, skipping",
				"deployment", latestDeployment.Name,
				"namespace", latestDeployment.Namespace)
			return nil
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestDeployment.DeepCopy()

		// Restore original replica count
		replicas := int32(originalReplicas)
		latestDeployment.Spec.Replicas = &replicas

		// Restore original terminationGracePeriodSeconds if exists
		if originalGraceStr, ok := latestDeployment.Annotations[appsv1alpha1.AnnotationOriginalTerminationGracePeriod]; ok {
			if originalGraceStr == nilAnnotationValue {
				latestDeployment.Spec.Template.Spec.TerminationGracePeriodSeconds = nil
			} else {
				val, err := strconv.ParseInt(originalGraceStr, 10, 64)
				if err == nil {
					latestDeployment.Spec.Template.Spec.TerminationGracePeriodSeconds = &val
				}
			}
			delete(latestDeployment.Annotations, appsv1alpha1.AnnotationOriginalTerminationGracePeriod)
		}

		// Remove the annotation
		delete(latestDeployment.Annotations, appsv1alpha1.AnnotationOriginalReplicas)

		// Use Patch with MergeFrom to be resilient to concurrent status updates
		return r.Patch(ctx, latestDeployment, client.MergeFrom(patchBase))
	})
}

// resumeStatefulSet restores the statefulset replicas from the annotation
func (r *NamespaceLifecyclePolicyReconciler) resumeStatefulSet(ctx context.Context, sts *appsv1.StatefulSet) error {
	log := ctrl.Log

	// Check if there's a stored original replica count
	originalReplicasStr, exists := sts.Annotations[appsv1alpha1.AnnotationOriginalReplicas]
	if !exists {
		log.V(1).Info("Skipping resume: statefulset was not frozen",
			"statefulset", sts.Name,
			"namespace", sts.Namespace,
			"reason", "StatefulSet has not been frozen by this operator")
		return nil
	}

	originalReplicas, err := strconv.Atoi(originalReplicasStr)
	if err != nil {
		return err
	}

	// Use retry to handle concurrent updates
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestSts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, latestSts); err != nil {
			return err
		}

		// Check again if annotation still exists (might have been resumed already)
		if _, exists := latestSts.Annotations[appsv1alpha1.AnnotationOriginalReplicas]; !exists {
			log.V(1).Info("StatefulSet already resumed, skipping",
				"statefulset", latestSts.Name,
				"namespace", latestSts.Namespace)
			return nil
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestSts.DeepCopy()

		// Restore original replica count
		replicas := int32(originalReplicas)
		latestSts.Spec.Replicas = &replicas

		// Restore original terminationGracePeriodSeconds if exists
		if originalGraceStr, ok := latestSts.Annotations[appsv1alpha1.AnnotationOriginalTerminationGracePeriod]; ok {
			if originalGraceStr == nilAnnotationValue {
				latestSts.Spec.Template.Spec.TerminationGracePeriodSeconds = nil
			} else {
				val, err := strconv.ParseInt(originalGraceStr, 10, 64)
				if err == nil {
					latestSts.Spec.Template.Spec.TerminationGracePeriodSeconds = &val
				}
			}
			delete(latestSts.Annotations, appsv1alpha1.AnnotationOriginalTerminationGracePeriod)
		}

		// Remove the annotation
		delete(latestSts.Annotations, appsv1alpha1.AnnotationOriginalReplicas)

		// Use Patch with MergeFrom to be resilient to concurrent status updates
		return r.Patch(ctx, latestSts, client.MergeFrom(patchBase))
	})
}

// updateStatus updates the policy status with phase, message and lastHandledOperationId
func (r *NamespaceLifecyclePolicyReconciler) updateStatus(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy, phase appsv1alpha1.Phase, message string, isStartupOperation bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
		if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
			return err
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestPolicy.DeepCopy()

		latestPolicy.Status.Phase = phase
		latestPolicy.Status.Message = message

		// Only update LastHandledOperationId for user-initiated operations
		if !isStartupOperation {
			latestPolicy.Status.LastHandledOperationId = policy.Spec.OperationId
		}

		// Use Patch for status to be resilient to concurrent updates
		return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
	})
}

// ApplyStartupPolicy applies the startup policy action to the namespace
// This is called once during operator startup for each policy
func (r *NamespaceLifecyclePolicyReconciler) ApplyStartupPolicy(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy) error {
	// Use a clean logger without framework metadata noise
	log := logf.Log.WithValues("policy", policy.Name)
	ctx = logf.IntoContext(ctx, log)

	// Record timestamp - set this at the very beginning
	now := metav1.Now()
	policy.Status.LastStartupAt = &now

	// Skip if startup policy is Ignore
	if policy.Spec.StartupPolicy == appsv1alpha1.StartupPolicyIgnore {
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				return err
			}
			patchBase := latestPolicy.DeepCopy()
			latestPolicy.Status.LastStartupAction = "SKIPPED_IGNORE"
			latestPolicy.Status.LastStartupAt = &now
			return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
		})
	}

	// Node readiness check
	if policy.Spec.StartupNodeReadinessPolicy != nil && policy.Spec.StartupNodeReadinessPolicy.Enabled {
		log.Info("Node readiness check enabled, waiting for nodes...",
			"policy", policy.Name)

		readyNodes, secondsWaited, err := r.waitForNodesReady(ctx, policy.Spec.StartupNodeReadinessPolicy)
		if err != nil {
			log.Error(err, "Error while waiting for nodes")
			// Continue anyway - we don't fail startup
		}

		// Record metrics
		policy.Status.StartupReadyNodes = &readyNodes
		policy.Status.StartupNodesWaited = &secondsWaited

		log.Info("Node readiness check completed",
			"policy", policy.Name,
			"readyNodes", readyNodes,
			"secondsWaited", secondsWaited)
	}

	// Determine desired phase based on startup policy
	var desiredPhase appsv1alpha1.Phase
	var action appsv1alpha1.LifecycleAction
	switch policy.Spec.StartupPolicy {
	case appsv1alpha1.StartupPolicyFreeze:
		desiredPhase = appsv1alpha1.PhaseFrozen
		action = appsv1alpha1.LifecycleActionFreeze
	case appsv1alpha1.StartupPolicyResume:
		desiredPhase = appsv1alpha1.PhaseResumed
		action = appsv1alpha1.LifecycleActionResume
	default:
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				return err
			}
			patchBase := latestPolicy.DeepCopy()
			latestPolicy.Status.LastStartupAction = "SKIPPED_UNKNOWN_POLICY"
			latestPolicy.Status.LastStartupAt = &now
			return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
		})
	}

	// NOTE: We do NOT check if Phase == desiredPhase and skip
	// Reason: Phase can be stale (from before operator restart)
	// Better to always apply startup policy - the freeze/resume functions are idempotent anyway
	// They will skip if workloads are already in the desired state

	// Apply startupResumeDelay if this is a Resume startup action
	// Set status fields instead of annotations - cleaner and follows Kubernetes best practices
	if action == appsv1alpha1.LifecycleActionResume && policy.Spec.StartupResumeDelay.Duration > 0 {
		log.Info("⏱️ Startup resume delay configured for startup policy - setting status for Reconcile loop",
			"delay", policy.Spec.StartupResumeDelay.Duration,
			"targetNamespace", policy.Spec.TargetNamespace)

		// Update status with retry to handle concurrent updates
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			// Fetch latest version to avoid conflict errors
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				return err
			}

			// Create a patch helper
			patchBase := latestPolicy.DeepCopy()

			// Mark as pending startup resume in status
			latestPolicy.Status.PendingStartupResume = true
			latestPolicy.Status.StartupResumeDelayStartedAt = &now
			latestPolicy.Status.Phase = appsv1alpha1.PhaseIdle
			latestPolicy.Status.Message = fmt.Sprintf("Waiting %s before starting startup Resume", policy.Spec.StartupResumeDelay.Duration)
			latestPolicy.Status.LastStartupAt = &now
			latestPolicy.Status.LastStartupAction = "RESUME_DELAYED"

			return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
		}); err != nil {
			log.Error(err, "Failed to update status for delayed startup resume")
			return err
		}

		log.Info("✅ Status updated successfully: PendingStartupResume=true", "policy", policy.Name)

		// Return success - Reconcile loop will handle the actual resume after delay
		return nil
	}

	log.Info("Applying startup policy",
		"policy", policy.Name,
		"startupPolicy", policy.Spec.StartupPolicy,
		"currentPhase", policy.Status.Phase,
		"desiredPhase", desiredPhase,
		"targetNamespace", policy.Spec.TargetNamespace)

	// Check if target namespace exists
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.TargetNamespace}, namespace); err != nil {
		if errors.IsNotFound(err) {
			policy.Status.LastStartupAction = "SKIPPED_NAMESPACE_NOT_FOUND"
			if err := r.Status().Update(ctx, policy); err != nil {
				log.Error(err, "Failed to update status")
			}
			log.Info("FAILED: Target namespace not found",
				"policy", policy.Name,
				"targetNamespace", policy.Spec.TargetNamespace)
			return nil // Don't fail, just skip
		}
		return err
	}

	// List resources
	deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		log.Error(err, "Failed to list deployments during startup")
		return err
	}

	statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		log.Error(err, "Failed to list statefulsets during startup")
		return err
	}

	log.Info("Startup policy: found resources",
		"deployments", len(deployments.Items),
		"statefulsets", len(statefulSets.Items))

	// Apply action
	switch action {
	case appsv1alpha1.LifecycleActionFreeze:
		for i := range deployments.Items {
			deployment := &deployments.Items[i]
			if err := r.freezeDeployment(ctx, deployment, policy); err != nil {
				log.Error(err, "Failed to freeze deployment during startup", "name", deployment.Name)
			}
		}
		for i := range statefulSets.Items {
			sts := &statefulSets.Items[i]
			if err := r.freezeStatefulSet(ctx, sts, policy); err != nil {
				log.Error(err, "Failed to freeze statefulset during startup", "name", sts.Name)
			}
		}
		policy.Status.Phase = appsv1alpha1.PhaseFrozen
		policy.Status.LastStartupAction = "FREEZE_APPLIED"
		// NOTE: Do NOT set LastHandledOperationId here!
		// Startup policy is independent of manual operations (spec.action/operationId)
		log.Info("⏸️ Startup policy applied: frozen", "policy", policy.Name)
	case appsv1alpha1.LifecycleActionResume:
		// Very prominent log when startup resume begins
		priority := policy.Spec.StartupResumePriority
		if priority == 0 {
			priority = 100
		}
		log.Info("🚀🚀🚀 ========== STARTUP RESUME OPERATION STARTING ========== 🚀🚀🚀",
			"policy", policy.Name,
			"targetNamespace", policy.Spec.TargetNamespace,
			"startupResumePriority", priority,
			"startupResumeDelay", policy.Spec.StartupResumeDelay.Duration,
			"workloads", fmt.Sprintf("%d deployments, %d statefulsets", len(deployments.Items), len(statefulSets.Items)))

		// Check pre-conditions before resuming (for startup resume)
		if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.Enabled {
			// Check if pre-conditions should block the priority chain
			// Default to true if not specified (nil)
			blockPriorityChain := true
			if policy.Spec.PreConditions.BlockPriorityChain != nil {
				blockPriorityChain = *policy.Spec.PreConditions.BlockPriorityChain
			}

			log.Info("Checking pre-conditions before startup resume",
				"appReadinessChecks", len(policy.Spec.PreConditions.AppReadinessChecks),
				"healthEndpointChecks", len(policy.Spec.PreConditions.HealthEndpointChecks),
				"blockPriorityChain", blockPriorityChain)

			if blockPriorityChain {
				// Blocking mode: wait for pre-conditions synchronously
				if err := r.waitForPreConditions(ctx, policy, true); err != nil {
					// Check if cancelled due to Freeze action
					if strings.Contains(err.Error(), "cancelled") {
						log.Info("Pre-conditions cancelled - action changed, returning to let reconcile handle new action")
						return nil // Return nil to let the next reconcile handle the new action
					}
					// Pre-conditions failed - set phase to Failed
					if strings.Contains(err.Error(), "timeout") {
						log.Error(err, "Pre-conditions timeout reached during startup resume")
						policy.Status.Phase = appsv1alpha1.PhaseFailed
						policy.Status.Message = fmt.Sprintf("Startup resume failed: pre-conditions timeout: %v", err)
						policy.Status.LastStartupAction = "RESUME_FAILED_PRECONDITIONS_TIMEOUT"
					} else {
						log.Error(err, "Failed to check pre-conditions during startup resume")
						policy.Status.Phase = appsv1alpha1.PhaseFailed
						policy.Status.Message = fmt.Sprintf("Startup resume failed: pre-conditions check failed: %v", err)
						policy.Status.LastStartupAction = "RESUME_FAILED_PRECONDITIONS_ERROR"
					}
					return err
				}

				log.Info("All pre-conditions passed, proceeding with startup resume")
			} else {
				// Non-blocking mode: check pre-conditions once, if passed proceed, otherwise return and let reconcile loop handle it
				log.Info("Pre-conditions enabled in non-blocking mode - checking once",
					"policy", policy.Name)

				allPassed, message, checkErr := r.checkPreConditions(ctx, policy)
				if checkErr != nil {
					log.Error(checkErr, "Failed to check pre-conditions during startup (non-blocking mode)")
					// Update status with retry to handle conflicts
					if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
						latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
						if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
							return getErr
						}
						now := metav1.Now()
						latestPolicy.Status.PreConditionsStatus = &appsv1alpha1.PreConditionsStatus{
							Checking:      true,
							LastCheckedAt: &now,
							Passed:        false,
							Message:       fmt.Sprintf("Error checking pre-conditions: %v", checkErr),
						}
						latestPolicy.Status.Phase = appsv1alpha1.PhaseResuming
						latestPolicy.Status.Message = "Waiting for pre-conditions (non-blocking mode)"
						latestPolicy.Status.PendingStartupResume = true
						return r.Status().Update(ctx, latestPolicy)
					}); updateErr != nil {
						log.Error(updateErr, "Failed to update policy status")
					}
					return nil
				}

				if allPassed {
					log.Info("All pre-conditions passed during startup (non-blocking mode), proceeding with resume")
					// Update pre-conditions status
					now := metav1.Now()
					status := &appsv1alpha1.PreConditionsStatus{
						Checking:      false,
						LastCheckedAt: &now,
						Passed:        true,
						Message:       "All pre-conditions passed",
					}
					if updateErr := r.updatePreConditionsStatus(ctx, policy, status); updateErr != nil {
						log.Error(updateErr, "Failed to update pre-conditions status")
					}
					// Continue with resume below
				} else {
					// Pre-conditions not ready yet
					log.Info("⏳ Pre-conditions not ready yet (non-blocking startup)",
						"policy", policy.Name,
						"message", message)
					// Update status with retry to handle conflicts
					if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
						latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
						if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
							return getErr
						}
						now := metav1.Now()
						latestPolicy.Status.PreConditionsStatus = &appsv1alpha1.PreConditionsStatus{
							Checking:      true,
							LastCheckedAt: &now,
							Passed:        false,
							Message:       message,
						}
						latestPolicy.Status.Phase = appsv1alpha1.PhaseResuming
						latestPolicy.Status.Message = "Waiting for pre-conditions (non-blocking mode - other policies continue)"
						latestPolicy.Status.PendingStartupResume = true
						return r.Status().Update(ctx, latestPolicy)
					}); updateErr != nil {
						log.Error(updateErr, "Failed to update policy status")
					}
					return nil
				}
			}
		}

		// Check if adaptive throttling is enabled
		if policy.Spec.AdaptiveThrottling != nil && policy.Spec.AdaptiveThrottling.Enabled {
			log.Info("🚀 Startup policy: using adaptive throttling for resume",
				"policy", policy.Name,
				"workloads", len(deployments.Items)+len(statefulSets.Items))

			if err := r.resumeWithAdaptiveThrottling(ctx, policy, deployments, statefulSets, true); err != nil {
				if err.Error() == "operation aborted due to manual override" {
					log.Info("🛑 Startup resume aborted due to manual override", "policy", policy.Name)
					return nil
				}
				log.Error(err, "Failed to resume with adaptive throttling during startup")
				return err
			}
		} else {
			// Fallback: resume all workloads immediately (old behavior)
			log.Info("⚡ Startup policy: resuming all workloads immediately (no throttling)",
				"policy", policy.Name,
				"workloads", len(deployments.Items)+len(statefulSets.Items))

			for i := range deployments.Items {
				deployment := &deployments.Items[i]
				if err := r.resumeDeployment(ctx, deployment); err != nil {
					log.Error(err, "Failed to resume deployment during startup", "name", deployment.Name)
				}
			}
			for i := range statefulSets.Items {
				sts := &statefulSets.Items[i]
				if err := r.resumeStatefulSet(ctx, sts); err != nil {
					log.Error(err, "Failed to resume statefulset during startup", "name", sts.Name)
				}
			}
		}
		policy.Status.Phase = appsv1alpha1.PhaseResumed
		// Always update message to indicate startup resume was applied
		if policy.Status.AdaptiveProgress != nil && policy.Status.AdaptiveProgress.Message != "" {
			// Use adaptive throttling message with startup resume prefix
			policy.Status.Message = fmt.Sprintf("Startup resume applied: %s", policy.Status.AdaptiveProgress.Message)
		} else {
			// Standard startup resume message
			policy.Status.Message = fmt.Sprintf("Startup resume applied: completed successfully (%d deployments, %d statefulsets)",
				len(deployments.Items), len(statefulSets.Items))
		}
		policy.Status.LastResumeAt = &now
		policy.Status.LastStartupAction = "RESUME_APPLIED"
		// NOTE: Do NOT set LastHandledOperationId here!
		// Startup policy is independent of manual operations (spec.action/operationId)
		// Setting it here would incorrectly mark manual operations as handled
		log.Info("✅ Startup policy applied: resumed", "policy", policy.Name)
	}

	// Update status after applying - use retry to handle conflicts
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version to avoid conflict errors
		latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
		if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
			return err
		}

		// Apply all status changes to the latest version
		latestPolicy.Status.Phase = policy.Status.Phase
		latestPolicy.Status.Message = policy.Status.Message
		latestPolicy.Status.LastStartupAt = policy.Status.LastStartupAt
		latestPolicy.Status.LastStartupAction = policy.Status.LastStartupAction
		// Do NOT copy LastHandledOperationId - startup ops don't consume operationId

		// Copy LastResumeAt if set
		if policy.Status.LastResumeAt != nil {
			latestPolicy.Status.LastResumeAt = policy.Status.LastResumeAt
		}

		// Copy adaptive progress if set (from adaptive throttling resume)
		// Adaptive throttling already updated this with final completion status
		if policy.Status.AdaptiveProgress != nil {
			latestPolicy.Status.AdaptiveProgress = policy.Status.AdaptiveProgress
		}

		// Copy node readiness metrics if they were set
		if policy.Status.StartupReadyNodes != nil {
			latestPolicy.Status.StartupReadyNodes = policy.Status.StartupReadyNodes
		}
		if policy.Status.StartupNodesWaited != nil {
			latestPolicy.Status.StartupNodesWaited = policy.Status.StartupNodesWaited
		}

		return r.Status().Update(ctx, latestPolicy)
	})
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// This implementation handles freezing/resuming Deployments and StatefulSets
// in a target namespace based on the policy configuration.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *NamespaceLifecyclePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Fetch the NamespaceLifecyclePolicy CR
	var policy appsv1alpha1.NamespaceLifecyclePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if errors.IsNotFound(err) {
			ctrl.Log.Info("NamespaceLifecyclePolicy deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		ctrl.Log.Error(err, "Failed to get NamespaceLifecyclePolicy")
		return ctrl.Result{}, err
	}

	// Use a clean logger without framework metadata noise
	log := logf.Log.WithValues("policy", policy.Name)
	ctx = logf.IntoContext(ctx, log)

	// Check for duplicate policies targeting the same namespace
	// We want to ensure only one policy manages a namespace at a time.
	// Resolution strategy: Older policy wins.
	policyList := &appsv1alpha1.NamespaceLifecyclePolicyList{}
	if err := r.List(ctx, policyList); err != nil {
		log.Error(err, "Failed to list NamespaceLifecyclePolicies")
		return ctrl.Result{}, err
	}

	for _, p := range policyList.Items {
		// Skip self
		if p.UID == policy.UID {
			continue
		}

		// Check if target namespace matches
		if p.Spec.TargetNamespace == policy.Spec.TargetNamespace {
			// If duplicate is being deleted, ignore it
			if !p.DeletionTimestamp.IsZero() {
				continue
			}

			// If duplicate is already Failed, ignore it (treat as non-existent for conflict resolution)
			// This allows a new valid policy to take over if the old one was failed.
			if p.Status.Phase == appsv1alpha1.PhaseFailed {
				continue
			}

			// If current policy is newer than the existing one, or equal time but alphabetically later, fail current.
			// This ensures determinstic behavior where one policy stays active and others fail.
			if policy.CreationTimestamp.After(p.CreationTimestamp.Time) ||
				(policy.CreationTimestamp.Equal(&p.CreationTimestamp) && policy.Name > p.Name) {

				msg := fmt.Sprintf("Conflict: Namespace '%s' is already managed by policy '%s'",
					policy.Spec.TargetNamespace, p.Name)
				log.Info("FAILED: Policy conflict detected",
					"policy", policy.Name,
					"targetNamespace", policy.Spec.TargetNamespace,
					"conflictsWith", p.Name,
					"reason", "Namespace already managed by another policy")

				if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed, msg, false); err != nil {
					log.Error(err, "Failed to update status for duplicate policy")
					return ctrl.Result{}, err
				}
				// Stop reconciliation
				return ctrl.Result{}, nil
			}
		}
	}

	// Handle pending startup resume with delay using status fields
	if policy.Status.PendingStartupResume {
		// 1. Check for manual operation override during startup delay
		// Only abort if there's a NEW/PENDING manual operation (spec.operationId != status.lastHandledOperationId)
		// Stale actions (already handled) should not block startup resume.
		isManualPending := !r.shouldSkipOperation(&policy)

		if isManualPending {
			log.Info("⚠️ Cancelling startup resume due to pending manual operation",
				"action", policy.Spec.Action,
				"operationId", policy.Spec.OperationId)

			// Update status to cancel the pending startup resume, but DON'T return
			// Let Reconcile continue to process the pending manual command
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, req.NamespacedName, latestPolicy); err != nil {
					return err
				}
				patchBase := latestPolicy.DeepCopy()
				latestPolicy.Status.PendingStartupResume = false
				latestPolicy.Status.LastStartupAction = "CANCELLED_BY_MANUAL_OVERRIDE"
				return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
			}); err != nil {
				log.Error(err, "Failed to cancel pending startup resume")
				return ctrl.Result{}, err
			}

			// Re-fetch the policy to get updated status, then continue to process manual command
			if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
				return ctrl.Result{}, err
			}
			// Don't return - fall through to process the manual command below
		} else {
			// No manual override - continue with startup resume delay logic
			if policy.Status.StartupResumeDelayStartedAt == nil {
				log.Error(fmt.Errorf("missing delay start time"), "Pending startup resume has no start time in status")
				// Clear the pending flag
				policy.Status.PendingStartupResume = false
				if err := r.Status().Update(ctx, &policy); err != nil {
					log.Error(err, "Failed to clear invalid pending startup resume")
				}
				return ctrl.Result{}, nil
			}

			elapsed := time.Since(policy.Status.StartupResumeDelayStartedAt.Time)
			if elapsed < policy.Spec.StartupResumeDelay.Duration {
				// Delay not yet complete
				remaining := policy.Spec.StartupResumeDelay.Duration - elapsed
				// Log at first check (when elapsed is very small) to show timer started
				if elapsed < 2*time.Second {
					log.Info("⏳ Startup resume delay timer started",
						"totalDelay", policy.Spec.StartupResumeDelay.Duration,
						"policy", policy.Name,
						"targetNamespace", policy.Spec.TargetNamespace)
				}
				log.V(1).Info("Startup resume delay in progress",
					"elapsed", elapsed,
					"remaining", remaining,
					"policy", policy.Name)
				return ctrl.Result{RequeueAfter: remaining}, nil
			}

			// Delay complete! Perform the startup resume now
			log.Info("🚀 Startup resume delay completed - executing resume",
				"delay", policy.Spec.StartupResumeDelay.Duration,
				"policy", policy.Name,
				"targetNamespace", policy.Spec.TargetNamespace)

			// Execute the resume operation
			deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
			if err != nil {
				log.Error(err, "Failed to list deployments for startup resume")
				return ctrl.Result{}, err
			}

			statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
			if err != nil {
				log.Error(err, "Failed to list statefulsets for startup resume")
				return ctrl.Result{}, err
			}

			// Very prominent log when delayed startup resume begins
			priority := policy.Spec.StartupResumePriority
			if priority == 0 {
				priority = 100
			}
			log.Info("🚀🚀🚀 ========== STARTUP RESUME OPERATION STARTING (AFTER DELAY) ========== 🚀🚀🚀",
				"policy", policy.Name,
				"targetNamespace", policy.Spec.TargetNamespace,
				"startupResumePriority", priority,
				"startupResumeDelay", policy.Spec.StartupResumeDelay.Duration,
				"workloads", fmt.Sprintf("%d deployments, %d statefulsets", len(deployments.Items), len(statefulSets.Items)))

			// Check pre-conditions before resuming (for delayed startup resume)
			if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.Enabled {
				// Check if pre-conditions should block the priority chain
				// Default to true if not specified (nil)
				blockPriorityChain := true
				if policy.Spec.PreConditions.BlockPriorityChain != nil {
					blockPriorityChain = *policy.Spec.PreConditions.BlockPriorityChain
				}

				log.Info("Checking pre-conditions before delayed startup resume",
					"appReadinessChecks", len(policy.Spec.PreConditions.AppReadinessChecks),
					"healthEndpointChecks", len(policy.Spec.PreConditions.HealthEndpointChecks),
					"blockPriorityChain", blockPriorityChain)

				if blockPriorityChain {
					// Blocking mode: wait for pre-conditions synchronously
					if err := r.waitForPreConditions(ctx, &policy, true); err != nil {
						// Check if cancelled due to Freeze action
						if strings.Contains(err.Error(), "cancelled") {
							log.Info("Pre-conditions cancelled during delayed startup resume - action changed")
							return ctrl.Result{Requeue: true}, nil // Requeue to handle new action
						}
						// Check if it's a timeout error
						if strings.Contains(err.Error(), "timeout") {
							log.Error(err, "Pre-conditions timeout reached during delayed startup resume")
							policy.Status.Phase = appsv1alpha1.PhaseFailed
							policy.Status.Message = fmt.Sprintf("Delayed startup resume failed: pre-conditions timeout: %v", err)
							policy.Status.LastStartupAction = "RESUME_FAILED_PRECONDITIONS_TIMEOUT"
							policy.Status.PendingStartupResume = false
							if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
								latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
								if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
									return getErr
								}
								latestPolicy.Status = policy.Status
								return r.Status().Update(ctx, latestPolicy)
							}); updateErr != nil {
								log.Error(updateErr, "Failed to update status")
							}
							return ctrl.Result{}, nil // Don't retry on timeout
						}
						log.Error(err, "Failed to check pre-conditions during delayed startup resume")
						policy.Status.Phase = appsv1alpha1.PhaseFailed
						policy.Status.Message = fmt.Sprintf("Delayed startup resume failed: pre-conditions check failed: %v", err)
						policy.Status.LastStartupAction = "RESUME_FAILED_PRECONDITIONS_ERROR"
						policy.Status.PendingStartupResume = false
						if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
							latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
							if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
								return getErr
							}
							latestPolicy.Status = policy.Status
							return r.Status().Update(ctx, latestPolicy)
						}); updateErr != nil {
							log.Error(updateErr, "Failed to update status")
						}
						return ctrl.Result{}, err
					}

					log.Info("All pre-conditions passed, proceeding with delayed startup resume")
				} else {
					// Non-blocking mode: check pre-conditions once, if passed proceed, otherwise return and let reconcile loop handle it
					log.Info("Pre-conditions enabled in non-blocking mode (delayed) - checking once",
						"policy", policy.Name)

					allPassed, message, checkErr := r.checkPreConditions(ctx, &policy)
					if checkErr != nil {
						log.Error(checkErr, "Failed to check pre-conditions during delayed startup (non-blocking mode)")
						// Update status but don't fail - let reconcile loop retry
						now := metav1.Now()
						status := &appsv1alpha1.PreConditionsStatus{
							Checking:      true,
							LastCheckedAt: &now,
							Passed:        false,
							Message:       fmt.Sprintf("Error checking pre-conditions: %v", checkErr),
						}
						policy.Status.PreConditionsStatus = status
						policy.Status.Phase = appsv1alpha1.PhaseResuming
						policy.Status.Message = "Waiting for pre-conditions (non-blocking mode)"
						policy.Status.PendingStartupResume = true // Keep pending so reconcile continues
						if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
							latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
							if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
								return getErr
							}
							latestPolicy.Status = policy.Status
							return r.Status().Update(ctx, latestPolicy)
						}); updateErr != nil {
							log.Error(updateErr, "Failed to update status")
						}
						return ctrl.Result{RequeueAfter: time.Duration(policy.Spec.PreConditions.CheckInterval) * time.Second}, nil
					}

					if allPassed {
						log.Info("All pre-conditions passed during delayed startup (non-blocking mode), proceeding with resume")
						// Update pre-conditions status
						now := metav1.Now()
						status := &appsv1alpha1.PreConditionsStatus{
							Checking:      false,
							LastCheckedAt: &now,
							Passed:        true,
							Message:       "All pre-conditions passed",
						}
						policy.Status.PreConditionsStatus = status
						// Continue with resume below
					} else {
						// Pre-conditions not ready yet
						log.Info("Pre-conditions not ready yet (non-blocking mode delayed), reconcile loop will continue checking",
							"policy", policy.Name, "message", message)
						now := metav1.Now()
						status := &appsv1alpha1.PreConditionsStatus{
							Checking:      true,
							LastCheckedAt: &now,
							Passed:        false,
							Message:       message,
						}
						policy.Status.PreConditionsStatus = status
						policy.Status.Phase = appsv1alpha1.PhaseResuming
						policy.Status.Message = "Waiting for pre-conditions (non-blocking mode - other policies continue)"
						policy.Status.PendingStartupResume = true // Keep pending so reconcile continues
						if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
							latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
							if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
								return getErr
							}
							latestPolicy.Status = policy.Status
							return r.Status().Update(ctx, latestPolicy)
						}); updateErr != nil {
							log.Error(updateErr, "Failed to update status")
						}
						// Requeue to continue checking
						return ctrl.Result{RequeueAfter: time.Duration(policy.Spec.PreConditions.CheckInterval) * time.Second}, nil
					}
				}
			}

			// Check if adaptive throttling is enabled
			if policy.Spec.AdaptiveThrottling != nil && policy.Spec.AdaptiveThrottling.Enabled {
				log.Info("🚀 Executing startup resume with adaptive throttling",
					"policy", policy.Name,
					"workloads", len(deployments.Items)+len(statefulSets.Items))

				if err := r.resumeWithAdaptiveThrottling(ctx, &policy, deployments, statefulSets, true); err != nil {
					if err.Error() == "operation aborted due to manual override" {
						log.Info("🛑 Delayed startup resume aborted due to manual override", "policy", policy.Name)
						return ctrl.Result{}, nil
					}
					log.Error(err, "Failed to resume with adaptive throttling during delayed startup")
					return ctrl.Result{}, err
				}
			} else {
				// Resume without throttling
				log.Info("⚡ Executing startup resume without throttling",
					"policy", policy.Name,
					"workloads", len(deployments.Items)+len(statefulSets.Items))

				for i := range deployments.Items {
					deployment := &deployments.Items[i]
					if err := r.resumeDeployment(ctx, deployment); err != nil {
						log.Error(err, "Failed to resume deployment during delayed startup", "name", deployment.Name)
					}
				}
				for i := range statefulSets.Items {
					sts := &statefulSets.Items[i]
					if err := r.resumeStatefulSet(ctx, sts); err != nil {
						log.Error(err, "Failed to resume statefulset during delayed startup", "name", sts.Name)
					}
				}
			}

			// Update status with retry on conflict
			now := metav1.Now()
			policy.Status.Phase = appsv1alpha1.PhaseResumed
			// Always update message to indicate startup resume was applied
			if policy.Status.AdaptiveProgress != nil && policy.Status.AdaptiveProgress.Message != "" {
				// Use adaptive throttling message with startup resume prefix
				policy.Status.Message = fmt.Sprintf("Startup resume applied: %s", policy.Status.AdaptiveProgress.Message)
			} else {
				// Standard startup resume message
				policy.Status.Message = fmt.Sprintf("Startup resume applied: completed after delay (%d deployments, %d statefulsets)",
					len(deployments.Items), len(statefulSets.Items))
			}
			policy.Status.LastResumeAt = &now
			policy.Status.LastStartupAction = "RESUME_APPLIED"
			policy.Status.PendingStartupResume = false // Clear the pending flag

			// NOTE: Do NOT set LastHandledOperationId here!
			// Startup policy is independent of manual operations (spec.action/operationId)

			// Use retry to handle conflicts (adaptive throttling may have updated status)
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				// Fetch latest version
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
					return err
				}

				// Apply status changes to latest version
				latestPolicy.Status.Phase = policy.Status.Phase
				latestPolicy.Status.Message = policy.Status.Message
				latestPolicy.Status.LastResumeAt = policy.Status.LastResumeAt
				latestPolicy.Status.LastStartupAction = policy.Status.LastStartupAction
				// Do NOT copy LastHandledOperationId - startup ops don't consume operationId
				latestPolicy.Status.PendingStartupResume = false // Clear the pending flag

				// Copy adaptive progress if set (from adaptive throttling resume)
				// Adaptive throttling already updated this with final completion status
				if policy.Status.AdaptiveProgress != nil {
					latestPolicy.Status.AdaptiveProgress = policy.Status.AdaptiveProgress
				}

				return r.Status().Update(ctx, latestPolicy)
			}); err != nil {
				log.Error(err, "Failed to update status after delayed startup resume")
				return ctrl.Result{}, err
			}

			log.Info("✅ Delayed startup resume completed successfully", "policy", policy.Name)
			return ctrl.Result{}, nil
		}
	}

	// Check if this operation was already handled
	if r.shouldSkipOperation(&policy) {
		// Only check balancing if this reconcile was triggered by a node event
		hasNodeEvent := false
		if policy.Status.NodeReadyEventDetectedAt != nil {
			if policy.Status.NodeReadyEventHandledAt == nil ||
				policy.Status.NodeReadyEventDetectedAt.After(policy.Status.NodeReadyEventHandledAt.Time) {
				hasNodeEvent = true
			}
		}

		if hasNodeEvent {
			log.Info("Operation handled, checking for pod balancing due to node event",
				"operationId", policy.Spec.OperationId,
				"policy", policy.Name)

			if policy.Spec.BalancePods && policy.Status.LastResumeAt != nil {
				if shouldBalance := r.shouldPerformBalancing(&policy); shouldBalance {
					log.Info("✅ Triggering pod balancing",
						"policy", policy.Name)

					if err := r.performBalancing(ctx, &policy); err != nil {
						log.Error(err, "Failed to perform balancing")
					}
				} else {
					// Log when window expired
					elapsed := time.Since(policy.Status.LastResumeAt.Time)
					balanceWindow := time.Duration(policy.Spec.BalanceWindowSeconds) * time.Second
					if balanceWindow == 0 {
						balanceWindow = 10 * time.Minute
					}

					log.Info("⛔ Node became Ready but Balance window EXPIRED",
						"policy", policy.Name,
						"elapsed", fmt.Sprintf("%ds", int(elapsed.Seconds())),
						"window", fmt.Sprintf("%ds", int(balanceWindow.Seconds())))
				}
			}

			// Mark the node-ready event as handled in status
			now := metav1.Now()
			policy.Status.NodeReadyEventHandledAt = &now
			if err := r.Status().Update(ctx, &policy); err != nil {
				log.Error(err, "Failed to update node-ready handled status")
				return ctrl.Result{}, err
			}
		} else {
			// No node event and operation already handled - safe to skip
			log.V(1).Info("Skipping operation: already handled", "operationId", policy.Spec.OperationId)
		}

		return ctrl.Result{}, nil
	}

	// Check if target namespace exists
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.TargetNamespace}, namespace); err != nil {
		if errors.IsNotFound(err) {
			errMsg := fmt.Sprintf("Target namespace '%s' not found", policy.Spec.TargetNamespace)
			log.Info(errMsg)

			// Update status without setting lastHandledOperationId (allow retry when namespace is created)
			// DO NOT set LastHandledOperationId - we want to retry when namespace is created
			if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed, errMsg, false); err != nil {
				log.Error(err, "Failed to update status")
				return ctrl.Result{}, err
			}

			// Don't return error - namespace not existing is an expected state
			// Kubernetes will auto-reconcile when namespace is created
			return ctrl.Result{}, nil
		}
		// Other error (permissions, api server down, etc) - this should be retried
		log.Error(err, "Failed to get target namespace")

		if statusErr := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed, fmt.Sprintf("Failed to get namespace: %v", err), false); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}

		return ctrl.Result{}, err
	}

	// Check if there's a pending startup resume with pre-conditions being checked
	// This handles the case where startupPolicy=Resume but action=Freeze, and pre-conditions are still being checked
	if policy.Status.PreConditionsStatus != nil && policy.Status.PreConditionsStatus.Checking {
		// Check pre-conditions
		allPassed, message, err := r.checkPreConditions(ctx, &policy)
		if err != nil {
			log.Error(err, "Failed to check pre-conditions")
			// Update status with retry - might be temporary error
			errMsg := fmt.Sprintf("Error: %v", err)
			if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
					return getErr
				}
				now := metav1.Now()
				if latestPolicy.Status.PreConditionsStatus == nil {
					latestPolicy.Status.PreConditionsStatus = &appsv1alpha1.PreConditionsStatus{}
				}
				latestPolicy.Status.PreConditionsStatus.LastCheckedAt = &now
				latestPolicy.Status.PreConditionsStatus.Message = errMsg
				return r.Status().Update(ctx, latestPolicy)
			}); updateErr != nil {
				log.Error(updateErr, "Failed to update pre-conditions status")
			}
			// Requeue to retry
			checkInterval := 5 * time.Second
			if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.CheckInterval > 0 {
				checkInterval = time.Duration(policy.Spec.PreConditions.CheckInterval) * time.Second
			}
			return ctrl.Result{RequeueAfter: checkInterval}, nil
		}

		if !allPassed {
			// Pre-conditions not ready yet - update status and requeue
			log.Info("⏳ Pre-conditions check (pending startup resume)",
				"policy", policy.Name,
				"passed", false,
				"message", message)
			// Update status with retry to handle conflicts
			if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
					return getErr
				}
				now := metav1.Now()
				if latestPolicy.Status.PreConditionsStatus == nil {
					latestPolicy.Status.PreConditionsStatus = &appsv1alpha1.PreConditionsStatus{}
				}
				latestPolicy.Status.PreConditionsStatus.LastCheckedAt = &now
				latestPolicy.Status.PreConditionsStatus.Message = message
				return r.Status().Update(ctx, latestPolicy)
			}); updateErr != nil {
				log.Error(updateErr, "Failed to update pre-conditions status")
			}
			// Requeue to check again
			checkInterval := 5 * time.Second
			if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.CheckInterval > 0 {
				checkInterval = time.Duration(policy.Spec.PreConditions.CheckInterval) * time.Second
			}
			return ctrl.Result{RequeueAfter: checkInterval}, nil
		}

		// All pre-conditions passed - proceed with startup resume
		log.Info("✅ All pre-conditions passed, proceeding with startup resume",
			"policy", policy.Name)
		now := metav1.Now()
		policy.Status.PreConditionsStatus.Checking = false
		policy.Status.PreConditionsStatus.Passed = true
		policy.Status.PreConditionsStatus.LastCheckedAt = &now
		policy.Status.PreConditionsStatus.Message = "All pre-conditions passed"
		policy.Status.PendingStartupResume = false

		// Now perform the actual resume operation
		deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
		if err != nil {
			log.Error(err, "Failed to list deployments for startup resume")
			return ctrl.Result{}, err
		}
		statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
		if err != nil {
			log.Error(err, "Failed to list statefulsets for startup resume")
			return ctrl.Result{}, err
		}

		// Check if adaptive throttling is enabled
		if policy.Spec.AdaptiveThrottling != nil && policy.Spec.AdaptiveThrottling.Enabled {
			log.Info("🚀 Pre-conditions passed - resuming with adaptive throttling",
				"policy", policy.Name,
				"workloads", len(deployments.Items)+len(statefulSets.Items))

			if err := r.resumeWithAdaptiveThrottling(ctx, &policy, deployments, statefulSets, true); err != nil {
				if err.Error() == "operation aborted due to manual override" {
					log.Info("🛑 Resume aborted due to manual override", "policy", policy.Name)
					return ctrl.Result{}, nil
				}
				log.Error(err, "Failed to resume with adaptive throttling")
				return ctrl.Result{}, err
			}
		} else {
			// Resume all workloads immediately
			log.Info("⚡ Pre-conditions passed - resuming all workloads immediately",
				"policy", policy.Name,
				"workloads", len(deployments.Items)+len(statefulSets.Items))

			for i := range deployments.Items {
				deployment := &deployments.Items[i]
				if err := r.resumeDeployment(ctx, deployment); err != nil {
					log.Error(err, "Failed to resume deployment", "name", deployment.Name)
				}
			}
			for i := range statefulSets.Items {
				sts := &statefulSets.Items[i]
				if err := r.resumeStatefulSet(ctx, sts); err != nil {
					log.Error(err, "Failed to resume statefulset", "name", sts.Name)
				}
			}
		}

		// Update final status with retry to handle conflicts
		deploymentCount := len(deployments.Items)
		statefulSetCount := len(statefulSets.Items)
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
				return getErr
			}
			latestPolicy.Status.Phase = appsv1alpha1.PhaseResumed
			latestPolicy.Status.Message = fmt.Sprintf("Successfully resumed after pre-conditions passed (%d deployments, %d statefulsets)",
				deploymentCount, statefulSetCount)
			nowResume := metav1.Now()
			latestPolicy.Status.LastResumeAt = &nowResume
			if latestPolicy.Status.PreConditionsStatus != nil {
				latestPolicy.Status.PreConditionsStatus.Checking = false
				latestPolicy.Status.PreConditionsStatus.Passed = true
				latestPolicy.Status.PreConditionsStatus.LastCheckedAt = &nowResume
				latestPolicy.Status.PreConditionsStatus.Message = "All pre-conditions passed"
			}
			latestPolicy.Status.PendingStartupResume = false
			return r.Status().Update(ctx, latestPolicy)
		}); err != nil {
			log.Error(err, "Failed to update status after resume")
			return ctrl.Result{}, err
		}

		log.Info("✅ Startup resume completed after pre-conditions passed", "policy", policy.Name)
		return ctrl.Result{}, nil
	}

	log.Info("Preparing to execute operation", "action", policy.Spec.Action, "namespace", policy.Spec.TargetNamespace)

	// Update status to processing phase
	var phase appsv1alpha1.Phase
	if policy.Spec.Action == appsv1alpha1.LifecycleActionFreeze {
		phase = appsv1alpha1.PhaseFreezing
	} else {
		phase = appsv1alpha1.PhaseResuming
	}

	if err := r.updateStatus(ctx, &policy, phase, "Processing request", false); err != nil {
		log.Error(err, "Failed to update status to processing")
		return ctrl.Result{}, err
	}

	// List Deployments in target namespace with selector
	deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		log.Error(err, "Failed to list deployments")
		if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
			fmt.Sprintf("Failed to list deployments: %v", err), false); err != nil {
			log.Error(err, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	// List StatefulSets in target namespace with selector
	statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		log.Error(err, "Failed to list statefulsets")
		if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
			fmt.Sprintf("Failed to list statefulsets: %v", err), false); err != nil {
			log.Error(err, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	log.Info("Found resources",
		"deployments", len(deployments.Items),
		"statefulsets", len(statefulSets.Items))

	// Check if no resources found
	if len(deployments.Items) == 0 && len(statefulSets.Items) == 0 {
		msg := "No deployments or statefulsets found in namespace"
		if policy.Spec.Selector != nil {
			msg = "No resources matched the selector in namespace"
			log.Info(msg,
				"namespace", policy.Spec.TargetNamespace,
				"selector", policy.Spec.Selector)
		} else {
			log.Info(msg, "namespace", policy.Spec.TargetNamespace)
		}

		// Update status - not failed, just nothing to do
		phase := appsv1alpha1.PhaseFrozen
		if policy.Spec.Action == appsv1alpha1.LifecycleActionResume {
			phase = appsv1alpha1.PhaseResumed
		}
		if err := r.updateStatus(ctx, &policy, phase, msg, false); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}

		log.Info("No action taken, no resources found", "action", policy.Spec.Action)
		return ctrl.Result{}, nil
	}

	// Apply action
	switch policy.Spec.Action {
	case appsv1alpha1.LifecycleActionFreeze:
		log.Info("❄️ Freezing all resources in namespace", "namespace", policy.Spec.TargetNamespace)

		// Freeze all deployments
		for i := range deployments.Items {
			deployment := &deployments.Items[i]
			log.Info("Freezing deployment", "name", deployment.Name)
			if err := r.freezeDeployment(ctx, deployment, &policy); err != nil {
				log.Error(err, "Failed to freeze deployment", "name", deployment.Name)
				if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
					fmt.Sprintf("Failed to freeze deployment %s: %v", deployment.Name, err), false); err != nil {
					log.Error(err, "Failed to update status")
				}
				return ctrl.Result{}, err
			}
		}

		// Freeze all statefulsets
		for i := range statefulSets.Items {
			sts := &statefulSets.Items[i]
			log.Info("Freezing statefulset", "name", sts.Name)
			if err := r.freezeStatefulSet(ctx, sts, &policy); err != nil {
				log.Error(err, "Failed to freeze statefulset", "name", sts.Name)
				if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
					fmt.Sprintf("Failed to freeze statefulset %s: %v", sts.Name, err), false); err != nil {
					log.Error(err, "Failed to update status")
				}
				return ctrl.Result{}, err
			}
		}

		log.Info("✅ Successfully frozen all resources",
			"deployments", len(deployments.Items),
			"statefulsets", len(statefulSets.Items))

		// Update status to frozen
		if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFrozen,
			fmt.Sprintf("Successfully froze %d deployments and %d statefulsets",
				len(deployments.Items), len(statefulSets.Items)), false); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}

	case appsv1alpha1.LifecycleActionResume:
		// Check pre-conditions before resuming
		if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.Enabled {
			// Check if pre-conditions should block
			// Default to true if not specified (nil)
			blockPriorityChain := true
			if policy.Spec.PreConditions.BlockPriorityChain != nil {
				blockPriorityChain = *policy.Spec.PreConditions.BlockPriorityChain
			}

			log.Info("Checking pre-conditions before resume",
				"appReadinessChecks", len(policy.Spec.PreConditions.AppReadinessChecks),
				"healthEndpointChecks", len(policy.Spec.PreConditions.HealthEndpointChecks),
				"blockPriorityChain", blockPriorityChain)

			// Update status to Resuming phase while checking pre-conditions
			if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseResuming, "Checking pre-conditions before resume", false); err != nil {
				log.Error(err, "Failed to update status")
				return ctrl.Result{}, err
			}

			if blockPriorityChain {
				// Blocking mode: wait for pre-conditions synchronously
				if err := r.waitForPreConditions(ctx, &policy, false); err != nil {
					// Check if cancelled due to action change (e.g., Freeze)
					if strings.Contains(err.Error(), "cancelled") {
						log.Info("Pre-conditions cancelled - action changed, requeuing to handle new action")
						return ctrl.Result{Requeue: true}, nil // Requeue to handle new action
					}
					// Check if it's a timeout error
					if strings.Contains(err.Error(), "timeout") {
						log.Error(err, "Pre-conditions timeout reached")
						if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
							fmt.Sprintf("Pre-conditions timeout: %v", err), false); err != nil {
							log.Error(err, "Failed to update status")
						}
						return ctrl.Result{}, nil // Don't retry on timeout
					}
					log.Error(err, "Failed to check pre-conditions")
					if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
						fmt.Sprintf("Pre-conditions check failed: %v", err), false); err != nil {
						log.Error(err, "Failed to update status")
					}
					return ctrl.Result{}, err
				}

				log.Info("All pre-conditions passed, proceeding with resume")
			} else {
				// Non-blocking mode: check pre-conditions once, requeue if not ready
				allPassed, message, err := r.checkPreConditions(ctx, &policy)
				if err != nil {
					// Fatal error - set phase to Failed
					if strings.Contains(err.Error(), "timeout") {
						log.Error(err, "Pre-conditions timeout reached")
						if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
							fmt.Sprintf("Pre-conditions timeout: %v", err), false); err != nil {
							log.Error(err, "Failed to update status")
						}
						return ctrl.Result{}, nil // Don't retry on timeout
					}
					log.Error(err, "Failed to check pre-conditions")
					if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
						fmt.Sprintf("Pre-conditions check failed: %v", err), false); err != nil {
						log.Error(err, "Failed to update status")
					}
					return ctrl.Result{}, err
				}

				if !allPassed {
					// Pre-conditions not ready yet - update status and requeue
					now := metav1.Now()
					status := &appsv1alpha1.PreConditionsStatus{
						Checking:      true,
						LastCheckedAt: &now,
						Passed:        false,
						Message:       message,
					}
					if err := r.updatePreConditionsStatus(ctx, &policy, status); err != nil {
						log.Error(err, "Failed to update pre-conditions status")
					}
					// Requeue after checkInterval to check again
					checkInterval := time.Duration(policy.Spec.PreConditions.CheckInterval) * time.Second
					if checkInterval == 0 {
						checkInterval = 5 * time.Second
					}
					log.Info("⏳ Pre-conditions check (non-blocking)",
						"policy", policy.Name,
						"passed", false,
						"message", message,
						"nextCheckIn", checkInterval)
					return ctrl.Result{RequeueAfter: checkInterval}, nil
				}

				// All pre-conditions passed - update status and proceed
				now := metav1.Now()
				status := &appsv1alpha1.PreConditionsStatus{
					Checking:      false,
					LastCheckedAt: &now,
					Passed:        true,
					Message:       "All pre-conditions passed",
				}
				if err := r.updatePreConditionsStatus(ctx, &policy, status); err != nil {
					log.Error(err, "Failed to update pre-conditions status")
				}
				log.Info("All pre-conditions passed, proceeding with resume")
			}
		}

		// Check if adaptive throttling is enabled
		if policy.Spec.AdaptiveThrottling != nil && policy.Spec.AdaptiveThrottling.Enabled {
			// Use adaptive throttling
			log.Info("🚀 Resuming with adaptive throttling enabled",
				"initialBatchSize", policy.Spec.AdaptiveThrottling.InitialBatchSize,
				"deployments", len(deployments.Items),
				"statefulsets", len(statefulSets.Items))

			if err := r.resumeWithAdaptiveThrottling(ctx, &policy, deployments, statefulSets, false); err != nil {
				if err.Error() == "operation aborted due to manual override" {
					log.Info("🛑 Manual resume aborted due to subsequent manual override", "policy", policy.Name)
					return ctrl.Result{}, nil
				}
				log.Error(err, "Failed to resume with adaptive throttling")
				if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
					fmt.Sprintf("Failed to resume: %v", err), false); err != nil {
					log.Error(err, "Failed to update status")
				}
				return ctrl.Result{}, err
			}
		} else {
			// Use legacy resume (all at once)
			log.Info("⚡ Resuming without throttling (legacy mode)",
				"deployments", len(deployments.Items),
				"statefulsets", len(statefulSets.Items))

			// Resume all deployments
			for i := range deployments.Items {
				deployment := &deployments.Items[i]
				log.Info("Resuming deployment", "name", deployment.Name)
				if err := r.resumeDeployment(ctx, deployment); err != nil {
					log.Error(err, "Failed to resume deployment", "name", deployment.Name)
					if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
						fmt.Sprintf("Failed to resume deployment %s: %v", deployment.Name, err), false); err != nil {
						log.Error(err, "Failed to update status")
					}
					return ctrl.Result{}, err
				}
			}

			// Resume all statefulsets
			for i := range statefulSets.Items {
				sts := &statefulSets.Items[i]
				log.Info("Resuming statefulset", "name", sts.Name)
				if err := r.resumeStatefulSet(ctx, sts); err != nil {
					log.Error(err, "Failed to resume statefulset", "name", sts.Name)
					if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
						fmt.Sprintf("Failed to resume statefulset %s: %v", sts.Name, err), false); err != nil {
						log.Error(err, "Failed to update status")
					}
					return ctrl.Result{}, err
				}
			}
		}

		// Set LastResumeAt timestamp for pod balancing
		now := metav1.Now()
		// We need to update this field as well, but our updateStatus helper only updates Phase, Message and OperationId.
		// Since we need to update a custom field (LastResumeAt) securely, we should use a custom retry block here.

		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				return err
			}

			latestPolicy.Status.Phase = appsv1alpha1.PhaseResumed
			latestPolicy.Status.Message = fmt.Sprintf("Successfully resumed %d deployments and %d statefulsets",
				len(deployments.Items), len(statefulSets.Items))
			latestPolicy.Status.LastHandledOperationId = policy.Spec.OperationId
			latestPolicy.Status.LastResumeAt = &now
			// Clear adaptive progress after successful completion
			latestPolicy.Status.AdaptiveProgress = nil

			return r.Status().Update(ctx, latestPolicy)
		})

		if err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}
	}

	log.Info("Successfully processed NamespaceLifecyclePolicy", "action", policy.Spec.Action)
	return ctrl.Result{}, nil
}

// isNodeReady checks if a node is in Ready condition
func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// countTotalWorkerNodes counts the total number of worker nodes matching the selector (regardless of readiness)
func (r *NamespaceLifecyclePolicyReconciler) countTotalWorkerNodes(ctx context.Context, nodeSelector map[string]string) (int32, error) {
	log := logf.FromContext(ctx)

	// Default selector if not specified
	if len(nodeSelector) == 0 {
		nodeSelector = map[string]string{
			"node-role.kubernetes.io/worker": "",
		}
	}

	// List all nodes first, then filter by label selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		log.Error(err, "Failed to list nodes")
		return 0, err
	}

	// Filter nodes by selector
	totalCount := int32(0)
	for _, node := range nodeList.Items {
		matchesSelector := true
		for key, value := range nodeSelector {
			nodeValue, exists := node.Labels[key]
			if !exists || nodeValue != value {
				matchesSelector = false
				break
			}
		}
		if matchesSelector {
			totalCount++
		}
	}

	return totalCount, nil
}

// countReadyWorkerNodes counts the number of ready worker nodes matching the selector
func (r *NamespaceLifecyclePolicyReconciler) countReadyWorkerNodes(ctx context.Context, nodeSelector map[string]string) (int32, error) {
	log := logf.FromContext(ctx)

	// Default selector if not specified
	if len(nodeSelector) == 0 {
		nodeSelector = map[string]string{
			"node-role.kubernetes.io/worker": "",
		}
	}

	log.V(1).Info("Counting ready worker nodes", "nodeSelector", nodeSelector)

	// List all nodes first, then filter by label selector
	// This is more reliable than client.MatchingLabels with empty string values
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList); err != nil {
		log.Error(err, "Failed to list nodes")
		return 0, err
	}

	log.V(1).Info("Listed all nodes", "totalNodes", len(nodeList.Items))

	// Filter nodes by selector
	var filteredNodes []corev1.Node
	for _, node := range nodeList.Items {
		matchesSelector := true
		for key, value := range nodeSelector {
			nodeValue, exists := node.Labels[key]
			if !exists || nodeValue != value {
				matchesSelector = false
				break
			}
		}
		if matchesSelector {
			filteredNodes = append(filteredNodes, node)
		}
	}

	log.V(1).Info("Filtered nodes by selector", "matchingNodes", len(filteredNodes), "nodeSelector", nodeSelector)

	// Count ready nodes
	readyCount := int32(0)
	for _, node := range filteredNodes {
		ready := isNodeReady(&node)
		log.V(1).Info("Checking node", "name", node.Name, "ready", ready, "labels", node.Labels)
		if ready {
			readyCount++
		}
	}

	log.V(1).Info("Ready worker node count", "readyCount", readyCount, "totalChecked", len(filteredNodes))

	return readyCount, nil
}

// waitForNodesReady waits for minimum number of worker nodes to be ready
// Returns the number of ready nodes and seconds waited
func (r *NamespaceLifecyclePolicyReconciler) waitForNodesReady(
	ctx context.Context,
	policy *appsv1alpha1.StartupNodeReadinessPolicy,
) (readyNodes int32, secondsWaited int32, err error) {
	log := logf.Log.WithName("startup-check")

	// Get configuration with defaults
	timeout := int32(60) // default
	if policy.TimeoutSeconds > 0 {
		timeout = policy.TimeoutSeconds
	}

	minNodes := int32(1) // default

	// Use the required field directly - no default needed
	requireAll := policy.RequireAllNodes

	if requireAll {
		// Wait for ALL matching nodes
		totalNodes, err := r.countTotalWorkerNodes(ctx, policy.NodeSelector)
		if err != nil {
			log.Error(err, "Failed to count total worker nodes")
			// Fall back to default minNodes
			minNodes = 1
		} else {
			minNodes = totalNodes
			log.Info("requireAllNodes enabled, waiting for all matching nodes",
				"totalNodes", totalNodes,
				"nodeSelector", policy.NodeSelector)
		}
	} else {
		// Use minReadyNodes
		if policy.MinReadyNodes > 0 {
			minNodes = policy.MinReadyNodes
		}
		log.Info("requireAllNodes disabled, using minReadyNodes",
			"minReadyNodes", minNodes)
	}

	log.Info("Waiting for worker nodes to be ready",
		"minReadyNodes", minNodes,
		"requireAllNodes", requireAll,
		"timeoutSeconds", timeout,
		"nodeSelector", policy.NodeSelector)

	startTime := time.Now()
	ticker := time.NewTicker(2 * time.Second) // Check every 2 seconds
	defer ticker.Stop()

	timeoutChan := time.After(time.Duration(timeout) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()

		case <-timeoutChan:
			// Timeout reached, count final nodes and proceed
			finalCount, err := r.countReadyWorkerNodes(ctx, policy.NodeSelector)
			elapsed := int32(time.Since(startTime).Seconds())

			log.Info("⏱️  Node readiness timeout reached, proceeding with available nodes",
				"readyNodes", finalCount,
				"minReadyNodes", minNodes,
				"secondsWaited", elapsed)

			return finalCount, elapsed, err

		case <-ticker.C:
			// Check node count
			count, err := r.countReadyWorkerNodes(ctx, policy.NodeSelector)
			if err != nil {
				log.Error(err, "Failed to count ready nodes")
				continue
			}

			elapsed := int32(time.Since(startTime).Seconds())
			log.V(1).Info("Checking node readiness",
				"readyNodes", count,
				"minReadyNodes", minNodes,
				"elapsed", elapsed)

			// Check if minimum met
			if count >= minNodes {
				log.Info("✅ Minimum worker nodes ready",
					"readyNodes", count,
					"minReadyNodes", minNodes,
					"secondsWaited", elapsed)
				return count, elapsed, nil
			}
		}
	}
}

// mapNodeReadyToPolicy maps Node Ready/NotReady events to NamespaceLifecyclePolicy resources
func (r *NamespaceLifecyclePolicyReconciler) mapNodeReadyToPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	log := logf.Log.WithName("node-event")

	node, ok := obj.(*corev1.Node)
	if !ok {
		log.Error(fmt.Errorf("expected Node object"), "Invalid object type")
		return nil
	}

	nodeReady := isNodeReady(node)

	// Get the Ready condition to check recent transition
	var readyCondition *corev1.NodeCondition
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == corev1.NodeReady {
			readyCondition = &node.Status.Conditions[i]
			break
		}
	}

	// Check if this is a recent transition (within last 10 seconds)
	isRecentTransition := false
	if readyCondition != nil {
		transitionAge := time.Since(readyCondition.LastTransitionTime.Time)
		isRecentTransition = transitionAge < 10*time.Second
	}

	// Skip if not a recent transition (applies to both NotReady and Ready)
	// This prevents logging stale events from resyncs or old transitions
	if !isRecentTransition {
		return nil
	}

	// Handle recent NotReady transition
	if !nodeReady {
		log.Info("⚠️  Node transitioned to NotReady",
			"node", node.Name,
			"action", "Pod balancing may be triggered when node becomes Ready")
		return nil
	}

	// Handle recent Ready transition
	log.Info("🟢 Node transitioned to Ready",
		"node", node.Name,
		"action", "Checking policies for pod balancing")

	// List all NamespaceLifecyclePolicy resources with balancePods=true
	policyList := &appsv1alpha1.NamespaceLifecyclePolicyList{}
	if err := r.List(ctx, policyList); err != nil {
		log.Error(err, "Failed to list NamespaceLifecyclePolicy resources for node event")
		return nil
	}

	var requests []reconcile.Request
	var candidatePolicies []string

	for i := range policyList.Items {
		policy := &policyList.Items[i]
		if policy.Spec.BalancePods && policy.Status.LastResumeAt != nil {
			// Update status to mark this reconcile was triggered by node event
			now := metav1.Now()
			policy.Status.NodeReadyEventDetectedAt = &now

			if err := r.Status().Update(ctx, policy); err != nil {
				log.Error(err, "Failed to update node-ready status", "policy", policy.Name)
				continue
			}

			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      policy.Name,
					Namespace: policy.Namespace,
				},
			})
			candidatePolicies = append(candidatePolicies, policy.Name)
		}
	}

	// Log only when actually enqueuing policies
	if len(requests) > 0 {
		log.V(1).Info("Enqueuing policies for balancing",
			"node", node.Name,
			"policies", candidatePolicies)
	}

	return requests
}

// shouldPerformBalancing checks if balancing should be performed based on time window
func (r *NamespaceLifecyclePolicyReconciler) shouldPerformBalancing(policy *appsv1alpha1.NamespaceLifecyclePolicy) bool {
	if policy.Status.LastResumeAt == nil {
		return false
	}

	balanceWindow := time.Duration(policy.Spec.BalanceWindowSeconds) * time.Second
	if balanceWindow == 0 {
		balanceWindow = 10 * time.Minute // Default 10 minutes
	}

	elapsed := time.Since(policy.Status.LastResumeAt.Time)
	return elapsed < balanceWindow
}

// performBalancing triggers rolling restart on all deployments/statefulsets in target namespace
func (r *NamespaceLifecyclePolicyReconciler) performBalancing(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy) error {
	log := logf.Log.WithValues("policy", policy.Name)

	// List deployments
	deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		return err
	}

	// Trigger rolling restart for each deployment
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if err := r.triggerRollingRestart(ctx, deployment); err != nil {
			log.Error(err, "Failed to trigger rolling restart for deployment", "deployment", deployment.Name)
		}
	}

	// List statefulsets
	statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		return err
	}

	// Trigger rolling restart for each statefulset
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		if err := r.triggerRollingRestartSts(ctx, sts); err != nil {
			log.Error(err, "Failed to trigger rolling restart for statefulset", "statefulset", sts.Name)
		}
	}

	return nil
}

// triggerRollingRestart updates deployment pod template annotation to trigger rolling update
func (r *NamespaceLifecyclePolicyReconciler) triggerRollingRestart(ctx context.Context, deployment *appsv1.Deployment) error {
	log := logf.FromContext(ctx)

	// Create a patch helper
	patchBase := deployment.DeepCopy()

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}

	deployment.Spec.Template.Annotations["apps.ops.dev/restart-timestamp"] = time.Now().Format(time.RFC3339)

	log.Info("Triggering rolling restart for balanced pod distribution",
		"deployment", deployment.Name,
		"namespace", deployment.Namespace)

	return r.Patch(ctx, deployment, client.MergeFrom(patchBase))
}

// triggerRollingRestartSts updates statefulset pod template annotation to trigger rolling update
func (r *NamespaceLifecyclePolicyReconciler) triggerRollingRestartSts(ctx context.Context, sts *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx)

	// Create a patch helper
	patchBase := sts.DeepCopy()

	if sts.Spec.Template.Annotations == nil {
		sts.Spec.Template.Annotations = make(map[string]string)
	}

	sts.Spec.Template.Annotations["apps.ops.dev/restart-timestamp"] = time.Now().Format(time.RFC3339)

	log.Info("Triggering rolling restart for balanced pod distribution",
		"statefulset", sts.Name,
		"namespace", sts.Namespace)

	return r.Patch(ctx, sts, client.MergeFrom(patchBase))
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceLifecyclePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.NamespaceLifecyclePolicy{}, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Trigger on generation changes (spec updates)
				if e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() {
					return true
				}

				// Also trigger when PendingStartupResume or NodeReadyEventDetectedAt changes
				// This handles scenarios where status is updated but generation doesn't change
				oldPolicy, oldOK := e.ObjectOld.(*appsv1alpha1.NamespaceLifecyclePolicy)
				newPolicy, newOK := e.ObjectNew.(*appsv1alpha1.NamespaceLifecyclePolicy)
				if oldOK && newOK {
					// Trigger if PendingStartupResume changed from false to true
					if !oldPolicy.Status.PendingStartupResume && newPolicy.Status.PendingStartupResume {
						return true
					}

					// Trigger if PendingStartupResume is true and the delay start time changed
					// This is crucial for operator restarts where ApplyStartupPolicy resets this timestamp
					if newPolicy.Status.PendingStartupResume && newPolicy.Status.StartupResumeDelayStartedAt != nil {
						if oldPolicy.Status.StartupResumeDelayStartedAt == nil ||
							!newPolicy.Status.StartupResumeDelayStartedAt.Equal(oldPolicy.Status.StartupResumeDelayStartedAt) {
							return true
						}
					}

					// Trigger if a new node ready event was detected
					if newPolicy.Status.NodeReadyEventDetectedAt != nil {
						if oldPolicy.Status.NodeReadyEventDetectedAt == nil ||
							newPolicy.Status.NodeReadyEventDetectedAt.After(oldPolicy.Status.NodeReadyEventDetectedAt.Time) {
							return true
						}
					}
				}

				// Don't trigger on other status updates
				return false
			},
			CreateFunc: func(e event.CreateEvent) bool {
				// Reconcile on create events IF there is a pending startup resume
				// This ensures that after operator restart, policies already in PendingStartupResume
				// state are enqueued for reconciliation to start the delay timer.
				policy, ok := e.Object.(*appsv1alpha1.NamespaceLifecyclePolicy)
				if ok && policy.Status.PendingStartupResume {
					return true
				}
				return false
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Trigger on delete
				return true
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		})).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(r.mapNodeReadyToPolicy),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					oldNode := e.ObjectOld.(*corev1.Node)
					newNode := e.ObjectNew.(*corev1.Node)

					oldReady := isNodeReady(oldNode)
					newReady := isNodeReady(newNode)

					// Only trigger when Ready status actually changes
					return oldReady != newReady
				},
				CreateFunc: func(e event.CreateEvent) bool {
					// Don't trigger on operator startup for existing nodes
					// Only trigger when operator is running and a node transitions NotReady -> Ready
					return false
				},
				DeleteFunc:  func(e event.DeleteEvent) bool { return false },
				GenericFunc: func(e event.GenericEvent) bool { return false },
			}),
		).
		Named("namespacelifecyclepolicy").
		Complete(r)
}

// checkAppReadiness checks if a Deployment or StatefulSet is ready
// appRef format: "namespace.name" (e.g., "production.database")
// Returns true if the app is ready, false otherwise
func (r *NamespaceLifecyclePolicyReconciler) checkAppReadiness(ctx context.Context, appRef string) (bool, error) {
	log := logf.FromContext(ctx)

	// Parse "namespace.name" format
	parts := strings.Split(appRef, ".")
	if len(parts) != 2 {
		return false, fmt.Errorf("invalid app reference format: %s (expected 'namespace.name')", appRef)
	}

	namespace := parts[0]
	name := parts[1]

	// Try Deployment first
	deployment := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, deployment)
	if err == nil {
		// Deployment found, check readiness
		readyReplicas := deployment.Status.ReadyReplicas
		replicas := deployment.Status.Replicas

		// Check if all replicas are ready
		if replicas == 0 {
			log.V(1).Info("Deployment has 0 replicas", "namespace", namespace, "name", name)
			return false, nil
		}

		if readyReplicas != replicas {
			log.V(1).Info("Deployment not ready", "namespace", namespace, "name", name,
				"readyReplicas", readyReplicas, "replicas", replicas)
			return false, nil
		}

		// Check Available condition
		for _, condition := range deployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentAvailable {
				if condition.Status != corev1.ConditionTrue {
					log.V(1).Info("Deployment Available condition not True", "namespace", namespace, "name", name)
					return false, nil
				}
				log.V(1).Info("Deployment is ready", "namespace", namespace, "name", name)
				return true, nil
			}
		}

		// If Available condition not found but replicas match, consider it ready
		log.V(1).Info("Deployment is ready (replicas match, no Available condition)", "namespace", namespace, "name", name)
		return true, nil
	}

	// If Deployment not found, try StatefulSet
	if !errors.IsNotFound(err) {
		// Some other error occurred
		return false, fmt.Errorf("failed to get deployment %s/%s: %w", namespace, name, err)
	}

	statefulSet := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, statefulSet)
	if err != nil {
		if errors.IsNotFound(err) {
			// Resource doesn't exist yet - treat as "not ready" and keep waiting
			log.V(1).Info("Deployment or StatefulSet not found, waiting for it to be created",
				"namespace", namespace, "name", name)
			return false, nil
		}
		return false, fmt.Errorf("failed to get statefulset %s/%s: %w", namespace, name, err)
	}

	// StatefulSet found, check readiness
	readyReplicas := statefulSet.Status.ReadyReplicas
	replicas := statefulSet.Status.Replicas

	// Check if all replicas are ready
	if replicas == 0 {
		log.V(1).Info("StatefulSet has 0 replicas", "namespace", namespace, "name", name)
		return false, nil
	}

	if readyReplicas != replicas {
		log.V(1).Info("StatefulSet not ready", "namespace", namespace, "name", name,
			"readyReplicas", readyReplicas, "replicas", replicas)
		return false, nil
	}

	log.V(1).Info("StatefulSet is ready", "namespace", namespace, "name", name)
	return true, nil
}

// checkHealthEndpoint checks if a health endpoint returns a healthy status
func (r *NamespaceLifecyclePolicyReconciler) checkHealthEndpoint(ctx context.Context, check appsv1alpha1.HealthEndpointCheck) (bool, error) {
	log := logf.FromContext(ctx)

	// Default expected status codes
	expectedCodes := check.ExpectedStatusCodes
	if len(expectedCodes) == 0 {
		expectedCodes = []int32{200, 201, 202, 204}
	}

	// Default timeout
	timeout := time.Duration(check.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: timeout,
	}

	// Make GET request
	req, err := http.NewRequestWithContext(ctx, "GET", check.URL, nil)
	if err != nil {
		return false, fmt.Errorf("failed to create request for %s: %w", check.URL, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.V(1).Info("Health endpoint request failed", "url", check.URL, "error", err)
		return false, nil // Not ready, but not a fatal error
	}
	defer resp.Body.Close()

	// Check if status code is in expected list
	statusCode := int32(resp.StatusCode)
	for _, expectedCode := range expectedCodes {
		if statusCode == expectedCode {
			log.V(1).Info("Health endpoint is healthy", "url", check.URL, "statusCode", statusCode)
			return true, nil
		}
	}

	log.V(1).Info("Health endpoint returned unexpected status", "url", check.URL, "statusCode", statusCode, "expectedCodes", expectedCodes)
	return false, nil
}

// checkPreConditions checks all pre-conditions and returns true if all pass
// Returns error only for fatal errors (not for conditions not being ready)
func (r *NamespaceLifecyclePolicyReconciler) checkPreConditions(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy) (bool, string, error) {
	log := logf.FromContext(ctx)

	if policy.Spec.PreConditions == nil || !policy.Spec.PreConditions.Enabled {
		return true, "", nil
	}

	preConditions := policy.Spec.PreConditions

	// Check app readiness
	var failedApps []string
	for _, appRef := range preConditions.AppReadinessChecks {
		ready, err := r.checkAppReadiness(ctx, appRef)
		if err != nil {
			// Fatal error (e.g., resource not found)
			return false, "", fmt.Errorf("failed to check app readiness for %s: %w", appRef, err)
		}
		if !ready {
			failedApps = append(failedApps, appRef)
		}
	}

	// Check health endpoints
	var failedEndpoints []string
	for _, endpointCheck := range preConditions.HealthEndpointChecks {
		healthy, err := r.checkHealthEndpoint(ctx, endpointCheck)
		if err != nil {
			// Fatal error
			return false, "", fmt.Errorf("failed to check health endpoint %s: %w", endpointCheck.URL, err)
		}
		if !healthy {
			failedEndpoints = append(failedEndpoints, endpointCheck.URL)
		}
	}

	// Build status message
	var messages []string
	if len(failedApps) > 0 {
		messages = append(messages, fmt.Sprintf("waiting for apps: %s", strings.Join(failedApps, ", ")))
	}
	if len(failedEndpoints) > 0 {
		messages = append(messages, fmt.Sprintf("waiting for endpoints: %s", strings.Join(failedEndpoints, ", ")))
	}

	if len(messages) > 0 {
		message := strings.Join(messages, "; ")
		log.V(1).Info("Pre-conditions not met", "message", message)
		return false, message, nil
	}

	log.Info("All pre-conditions passed")
	return true, "All pre-conditions passed", nil
}

// waitForPreConditions waits for all pre-conditions to pass
// Returns error if timeout is reached or fatal error occurs
// isStartupOperation: if true, ignores spec.action changes (startup operations only respect spec.startupPolicy)
func (r *NamespaceLifecyclePolicyReconciler) waitForPreConditions(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy, isStartupOperation bool) error {
	log := logf.FromContext(ctx)

	if policy.Spec.PreConditions == nil || !policy.Spec.PreConditions.Enabled {
		return nil
	}

	preConditions := policy.Spec.PreConditions

	// Get check interval
	checkInterval := time.Duration(preConditions.CheckInterval) * time.Second
	if checkInterval == 0 {
		checkInterval = 5 * time.Second
	}

	// Get timeout
	var timeoutChan <-chan time.Time
	if preConditions.TimeoutSeconds > 0 {
		timeoutChan = time.After(time.Duration(preConditions.TimeoutSeconds) * time.Second)
	}

	// Update status to indicate checking
	now := metav1.Now()
	status := &appsv1alpha1.PreConditionsStatus{
		Checking:      true,
		LastCheckedAt: &now,
		Passed:        false,
		Message:       "Checking pre-conditions...",
	}

	if err := r.updatePreConditionsStatus(ctx, policy, status); err != nil {
		log.Error(err, "Failed to update pre-conditions status")
	}

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	log.Info("Starting pre-conditions check",
		"appReadinessChecks", len(preConditions.AppReadinessChecks),
		"healthEndpointChecks", len(preConditions.HealthEndpointChecks),
		"checkInterval", checkInterval,
		"timeoutSeconds", preConditions.TimeoutSeconds)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case <-timeoutChan:
			// Timeout reached - stop checking and update status
			now := metav1.Now()
			status.Checking = false
			status.Passed = false
			status.LastCheckedAt = &now
			status.Message = "Pre-conditions timeout reached"
			if err := r.updatePreConditionsStatus(ctx, policy, status); err != nil {
				log.Error(err, "Failed to update pre-conditions status")
			}
			return fmt.Errorf("pre-conditions timeout after %d seconds", preConditions.TimeoutSeconds)

		case <-ticker.C:
			// Re-fetch the policy to check if action has changed
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				log.Error(err, "Failed to re-fetch policy during pre-conditions check")
				// Continue with the old policy reference
			} else {
				// Only check for action changes during manual operations
				// During startup operations, spec.action is irrelevant - only spec.startupPolicy matters
				if !isStartupOperation {
					// Check if there's a NEW manual operation (operationId changed) with action=Freeze
					// Only cancel if this is a new operation, not a stale one
					hasNewOperation := latestPolicy.Spec.OperationId != "" &&
						latestPolicy.Spec.OperationId != latestPolicy.Status.LastHandledOperationId

					if hasNewOperation && latestPolicy.Spec.Action == appsv1alpha1.LifecycleActionFreeze {
						log.Info("🛑 New manual Freeze operation detected - cancelling pre-conditions wait",
							"policy", policy.Name,
							"newOperationId", latestPolicy.Spec.OperationId,
							"lastHandledOperationId", latestPolicy.Status.LastHandledOperationId)
						now := metav1.Now()
						status.Checking = false
						status.Passed = false
						status.LastCheckedAt = &now
						status.Message = "Pre-conditions check cancelled - new Freeze operation received"
						if updateErr := r.updatePreConditionsStatus(ctx, latestPolicy, status); updateErr != nil {
							log.Error(updateErr, "Failed to update pre-conditions status")
						}
						return fmt.Errorf("pre-conditions cancelled: new manual Freeze operation")
					}
				}

				// Check if pre-conditions were disabled
				if latestPolicy.Spec.PreConditions == nil || !latestPolicy.Spec.PreConditions.Enabled {
					log.Info("Pre-conditions disabled - cancelling wait", "policy", policy.Name)
					now := metav1.Now()
					status.Checking = false
					status.Passed = false
					status.LastCheckedAt = &now
					status.Message = "Pre-conditions check cancelled - disabled"
					if updateErr := r.updatePreConditionsStatus(ctx, latestPolicy, status); updateErr != nil {
						log.Error(updateErr, "Failed to update pre-conditions status")
					}
					return nil
				}

				// Update policy reference for checks
				policy = latestPolicy
			}

			// Check pre-conditions
			allPassed, message, err := r.checkPreConditions(ctx, policy)
			if err != nil {
				// Fatal error - stop checking and update status
				log.Error(err, "⚠️ Pre-conditions check failed with error", "policy", policy.Name)
				now := metav1.Now()
				status.Checking = false
				status.Passed = false
				status.LastCheckedAt = &now
				status.Message = fmt.Sprintf("Error checking pre-conditions: %v", err)
				if updateErr := r.updatePreConditionsStatus(ctx, policy, status); updateErr != nil {
					log.Error(updateErr, "Failed to update pre-conditions status")
				}
				return err
			}

			// Update status
			now := metav1.Now()
			status.LastCheckedAt = &now
			status.Message = message

			if allPassed {
				status.Checking = false
				status.Passed = true
				status.Message = "All pre-conditions passed"
				if err := r.updatePreConditionsStatus(ctx, policy, status); err != nil {
					log.Error(err, "Failed to update pre-conditions status")
				}
				log.Info("✅ All pre-conditions passed, proceeding with resume", "policy", policy.Name)
				return nil
			}

			// Log interval check (not passed yet)
			log.Info("⏳ Pre-conditions check (interval)",
				"policy", policy.Name,
				"passed", false,
				"message", message,
				"nextCheckIn", checkInterval)

			// Update status and continue waiting
			if err := r.updatePreConditionsStatus(ctx, policy, status); err != nil {
				log.Error(err, "Failed to update pre-conditions status")
			}
		}
	}
}

// updatePreConditionsStatus updates the pre-conditions status in the policy
func (r *NamespaceLifecyclePolicyReconciler) updatePreConditionsStatus(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy, status *appsv1alpha1.PreConditionsStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
		if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
			return err
		}

		patchBase := latestPolicy.DeepCopy()
		latestPolicy.Status.PreConditionsStatus = status

		return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
	})
}
