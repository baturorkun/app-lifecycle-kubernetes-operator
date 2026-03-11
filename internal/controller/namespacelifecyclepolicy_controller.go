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
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
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

	// Don't skip if pending startup resume AND there is a new/unhandled manual command.
	// (A handled operationId should still be skipped even when PendingStartupResume is true.)
	if policy.Status.PendingStartupResume && policy.Status.LastHandledOperationId != policy.Spec.OperationId {
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
func (r *NamespaceLifecyclePolicyReconciler) updateStatus(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy, phase appsv1alpha1.Phase, message string) error {
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

		// Always update LastHandledOperationId for manual operations
		// Startup operations should not call this helper with the user's operationId
		latestPolicy.Status.LastHandledOperationId = policy.Spec.OperationId

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

	// If requested action is Resume and the policy is already in Resumed phase, ignore it.
	if action == appsv1alpha1.LifecycleActionResume && policy.Status.Phase == appsv1alpha1.PhaseResumed {
		log.Info("⏩ Already resumed, no startup action needed", "policy", policy.Name)
		return retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				return err
			}
			patchBase := latestPolicy.DeepCopy()
			latestPolicy.Status.LastStartupAction = "NO_ACTION_ALREADY_RESUMED"
			latestPolicy.Status.LastStartupAt = &now
			return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
		})
	}

	// Apply startupResumeDelay if this is a Resume startup action
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
		// Apply freezeDelay if this is a Freeze startup action
		if policy.Spec.FreezeDelay.Duration > 0 {
			log.Info("⏱️ Startup freeze delay configured - setting status for Reconcile loop",
				"delay", policy.Spec.FreezeDelay.Duration,
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

				// Mark as pending freeze in status
				latestPolicy.Status.PendingFreeze = true
				latestPolicy.Status.FreezeDelayStartedAt = &now
				latestPolicy.Status.Phase = appsv1alpha1.PhaseIdle
				latestPolicy.Status.Message = fmt.Sprintf("Waiting %s before starting startup Freeze", policy.Spec.FreezeDelay.Duration)
				latestPolicy.Status.LastStartupAt = &now
				latestPolicy.Status.LastStartupAction = "FREEZE_DELAYED"

				return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
			}); err != nil {
				log.Error(err, "Failed to update status for delayed startup freeze")
				return err
			}

			log.Info("✅ Status updated successfully: PendingFreeze=true", "policy", policy.Name)

			// Return success - Reconcile loop will handle the actual freeze after delay
			return nil
		}

		// No delay - freeze immediately
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
		// FILTER: Only count and process workloads that were actually frozen
		deployments, statefulSets = r.filterWorkloadsRequiringResume(deployments, statefulSets)

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

		// NON-BLOCKING Pre-conditions handle:
		// If pre-conditions are enabled, we trigger a background check instead of blocking.
		if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.Enabled {
			log.Info("Startup policy: pre-conditions enabled - triggering background check", "policy", policy.Name)

			policy.Status.Phase = appsv1alpha1.PhaseResuming
			policy.Status.Message = "Startup: waiting for pre-conditions (background)"
			if policy.Status.PreConditionsStatus == nil {
				policy.Status.PreConditionsStatus = &appsv1alpha1.PreConditionsStatus{}
			}
			policy.Status.PreConditionsStatus.Checking = true
			policy.Status.PendingStartupResume = true // Ensure it stays in the startup flow
			policy.Status.StartupResumeDelayStartedAt = &now
			policy.Status.LastStartupAction = "STARTUP_PENDING_PRECONDITIONS"
			policy.Status.LastStartupAt = &now

			// Return here - the update at the end of ApplyStartupPolicy will save this status,
			// and the Reconcile loop will pick up the background check.
			return retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
					return err
				}
				patchBase := latestPolicy.DeepCopy()
				latestPolicy.Status.Phase = policy.Status.Phase
				latestPolicy.Status.Message = policy.Status.Message
				latestPolicy.Status.PreConditionsStatus = policy.Status.PreConditionsStatus
				latestPolicy.Status.PendingStartupResume = policy.Status.PendingStartupResume
				latestPolicy.Status.StartupResumeDelayStartedAt = policy.Status.StartupResumeDelayStartedAt
				latestPolicy.Status.LastStartupAction = policy.Status.LastStartupAction
				latestPolicy.Status.LastStartupAt = policy.Status.LastStartupAt
				return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
			})
		}

		// If no pre-conditions (or disabled), proceed with standard resume
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

				if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed, msg); err != nil {
					log.Error(err, "Failed to update status for duplicate policy")
					return ctrl.Result{}, err
				}
				// Stop reconciliation
				return ctrl.Result{}, nil
			}
		}
	}

	// Handle pending freeze with delay using status fields
	if policy.Status.PendingFreeze {
		// Check for manual operation override during freeze delay
		// Only abort if there's a NEW/PENDING manual operation
		isManualPending := !r.shouldSkipOperation(&policy)

		if isManualPending {
			log.Info("⚠️ Cancelling pending freeze due to pending manual operation",
				"action", policy.Spec.Action,
				"operationId", policy.Spec.OperationId)

			// Update status to cancel the pending freeze
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, req.NamespacedName, latestPolicy); err != nil {
					return err
				}
				patchBase := latestPolicy.DeepCopy()
				latestPolicy.Status.PendingFreeze = false
				latestPolicy.Status.LastStartupAction = "FREEZE_CANCELLED_BY_MANUAL_OVERRIDE"
				return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
			}); err != nil {
				log.Error(err, "Failed to cancel pending freeze")
				return ctrl.Result{}, err
			}

			// Re-fetch the policy to get updated status, then continue to process manual command
			if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
				return ctrl.Result{}, err
			}
			// Don't return - fall through to process the manual command below
		} else {
			// No manual override - continue with freeze delay logic
			if policy.Status.FreezeDelayStartedAt == nil {
				log.Error(fmt.Errorf("missing delay start time"), "Pending freeze has no start time in status")
				// Clear the pending flag
				policy.Status.PendingFreeze = false
				if err := r.Status().Update(ctx, &policy); err != nil {
					log.Error(err, "Failed to clear invalid pending freeze")
				}
				return ctrl.Result{}, nil
			}

			elapsed := time.Since(policy.Status.FreezeDelayStartedAt.Time)
			if elapsed < policy.Spec.FreezeDelay.Duration {
				// Delay not yet complete
				remaining := policy.Spec.FreezeDelay.Duration - elapsed
				// Log at first check (when elapsed is very small)
				if elapsed < 2*time.Second {
					log.Info("⏳ Freeze delay timer started",
						"totalDelay", policy.Spec.FreezeDelay.Duration,
						"policy", policy.Name,
						"targetNamespace", policy.Spec.TargetNamespace)
				}
				log.V(1).Info("Freeze delay in progress",
					"elapsed", elapsed,
					"remaining", remaining,
					"policy", policy.Name)
				return ctrl.Result{RequeueAfter: remaining}, nil
			}

			// Delay complete! Perform the freeze now
			log.Info("🧊 Freeze delay completed - executing freeze",
				"delay", policy.Spec.FreezeDelay.Duration,
				"policy", policy.Name,
				"targetNamespace", policy.Spec.TargetNamespace)

			// Execute the freeze operation
			deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
			if err != nil {
				log.Error(err, "Failed to list deployments for freeze")
				return ctrl.Result{}, err
			}

			statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
			if err != nil {
				log.Error(err, "Failed to list statefulsets for freeze")
				return ctrl.Result{}, err
			}

			log.Info("🧊🧊🧊 ========== FREEZE OPERATION STARTING (AFTER DELAY) ========== 🧊🧊🧊",
				"policy", policy.Name,
				"targetNamespace", policy.Spec.TargetNamespace,
				"freezePriority", policy.Spec.FreezePriority,
				"freezeDelay", policy.Spec.FreezeDelay.Duration,
				"workloads", fmt.Sprintf("%d deployments, %d statefulsets", len(deployments.Items), len(statefulSets.Items)))

			// Freeze all deployments
			for i := range deployments.Items {
				deployment := &deployments.Items[i]
				if err := r.freezeDeployment(ctx, deployment, &policy); err != nil {
					log.Error(err, "Failed to freeze deployment", "name", deployment.Name)
				}
			}

			// Freeze all statefulsets
			for i := range statefulSets.Items {
				sts := &statefulSets.Items[i]
				if err := r.freezeStatefulSet(ctx, sts, &policy); err != nil {
					log.Error(err, "Failed to freeze statefulset", "name", sts.Name)
				}
			}

			// Update status with retry on conflict
			now := metav1.Now()
			policy.Status.Phase = appsv1alpha1.PhaseFrozen
			policy.Status.Message = fmt.Sprintf("Freeze applied after delay: completed successfully (%d deployments, %d statefulsets)",
				len(deployments.Items), len(statefulSets.Items))
			policy.Status.LastFreezeAt = &now
			policy.Status.LastStartupAction = "FREEZE_APPLIED"
			policy.Status.PendingFreeze = false // Clear the pending flag

			// Use retry to handle conflicts
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				// Fetch latest version
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
					return err
				}

				// Apply status changes to latest version
				latestPolicy.Status.Phase = policy.Status.Phase
				latestPolicy.Status.Message = policy.Status.Message
				latestPolicy.Status.LastFreezeAt = policy.Status.LastFreezeAt
				latestPolicy.Status.LastStartupAction = policy.Status.LastStartupAction
				latestPolicy.Status.PendingFreeze = false // Clear the pending flag

				return r.Status().Update(ctx, latestPolicy)
			}); err != nil {
				log.Error(err, "Failed to update status after delayed freeze")
				return ctrl.Result{}, err
			}

			log.Info("✅ Delayed freeze completed successfully", "policy", policy.Name)
			return ctrl.Result{}, nil
		}
	}

	// Handle pending startup resume with delay using status fields
	if policy.Status.PendingStartupResume {
		// If a node failure is active, cancel the startup resume and fall through to
		// node failure handling below. Resuming into a partially-failed cluster would
		// bring up workloads on healthy nodes but leave the failed-node workloads broken.
		if policy.Spec.HandleNodeFailure &&
			policy.Status.NodeFailureEventDetectedAt != nil &&
			(policy.Status.NodeFailureEventHandledAt == nil ||
				policy.Status.NodeFailureEventDetectedAt.After(policy.Status.NodeFailureEventHandledAt.Time)) {

			log.Info("⚠️ Cancelling startup resume: node failure is active — will handle node failure instead",
				"policy", policy.Name,
				"failedNode", policy.Status.FailedNodeName)

			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, req.NamespacedName, latestPolicy); err != nil {
					return err
				}
				patchBase := latestPolicy.DeepCopy()
				latestPolicy.Status.PendingStartupResume = false
				latestPolicy.Status.LastStartupAction = "CANCELLED_BY_NODE_FAILURE"
				return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
			}); err != nil {
				log.Error(err, "Failed to cancel startup resume due to node failure")
				return ctrl.Result{}, err
			}

			// Re-fetch so the node failure branch below sees the updated status
			if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
				return ctrl.Result{}, err
			}
			// Fall through — the node failure branch handles the rest

		} else {
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

				// Non-blocking pre-conditions check
				if policy.Spec.PreConditions != nil && policy.Spec.PreConditions.Enabled {
					if policy.Status.PreConditionsStatus == nil || !policy.Status.PreConditionsStatus.Passed {
						log.Info("⏳ Startup resume waiting for pre-conditions to pass (background)",
							"policy", policy.Name,
							"status", "waiting")
						// Requeue to check again after a short delay
						return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
					}
					log.Info("✅ Pre-conditions passed, proceeding with delayed startup resume", "policy", policy.Name)
				}

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
				if policy.Status.FailedNodeName != "" {
					log.Info("🔄🔄🔄 ========== NODE FAILURE RECOVERY RESUME STARTING ========== 🔄🔄🔄",
						"policy", policy.Name,
						"targetNamespace", policy.Spec.TargetNamespace,
						"failedNode", policy.Status.FailedNodeName,
						"startupResumePriority", priority,
						"startupResumeDelay", policy.Spec.StartupResumeDelay.Duration,
						"workloads", fmt.Sprintf("%d deployments, %d statefulsets", len(deployments.Items), len(statefulSets.Items)))
				} else {
					log.Info("🚀🚀🚀 ========== STARTUP RESUME OPERATION STARTING (AFTER DELAY) ========== 🚀🚀🚀",
						"policy", policy.Name,
						"targetNamespace", policy.Spec.TargetNamespace,
						"startupResumePriority", priority,
						"startupResumeDelay", policy.Spec.StartupResumeDelay.Duration,
						"workloads", fmt.Sprintf("%d deployments, %d statefulsets", len(deployments.Items), len(statefulSets.Items)))
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
				// If a node failure is still active, immediately force-delete any pods that are
				// already Terminating on the failed node (Kubernetes only sets DeletionTimestamp
				// ~5min after NotReady, so some pods may already be stuck by now).
				// Then keep requeueing every 30s to catch any newly-terminating pods.
				if policy.Spec.HandleNodeFailure && policy.Status.FailedNodeName != "" {
					log.Info("🗑️ Force-deleting terminating pods after recovery resume",
						"policy", policy.Name,
						"failedNode", policy.Status.FailedNodeName)
					r.forceDeleteTerminatingPods(ctx, &policy, policy.Status.FailedNodeName)
					return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
				}
				return ctrl.Result{}, nil
			}
		} // end else: no node failure active, proceed with normal startup resume logic
	}

	// === NODE FAILURE HANDLING ===
	// This runs before shouldSkipOperation so node failures are always processed,
	// regardless of whether the current operationId has already been handled.
	hasNodeFailureEvent := policy.Spec.HandleNodeFailure &&
		policy.Status.NodeFailureEventDetectedAt != nil &&
		(policy.Status.NodeFailureEventHandledAt == nil ||
			policy.Status.NodeFailureEventDetectedAt.After(policy.Status.NodeFailureEventHandledAt.Time))

	if hasNodeFailureEvent {
		log.Info("🔴 Node failure event — handling scale-down of fully-local workloads",
			"policy", policy.Name,
			"failedNode", policy.Status.FailedNodeName)

		affectedWorkloads, err := r.handleNodeFailureEvent(ctx, &policy)
		if err != nil {
			log.Error(err, "Failed to handle node failure event")
			return ctrl.Result{}, err
		}

		now := metav1.Now()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latest := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
				return err
			}
			patchBase := latest.DeepCopy()
			latest.Status.NodeFailureEventHandledAt = &now
			latest.Status.AffectedWorkloads = affectedWorkloads
			// Block the normal Freeze action from re-running after requeue.
			if latest.Spec.OperationId != "" {
				latest.Status.LastHandledOperationId = latest.Spec.OperationId
			}
			// Trigger the existing PendingStartupResume path so workloads are
			// rescheduled on surviving nodes — no new code needed.
			latest.Status.PendingStartupResume = true
			latest.Status.StartupResumeDelayStartedAt = &now
			if len(affectedWorkloads) > 0 {
				setDegradedCondition(&latest.Status.Conditions, latest.Status.FailedNodeName, affectedWorkloads)
				log.Info("🔴 Namespace DEGRADED due to node failure",
					"policy", latest.Name,
					"failedNode", latest.Status.FailedNodeName,
					"affectedWorkloads", affectedWorkloads)
			}
			return r.Status().Patch(ctx, latest, client.MergeFrom(patchBase))
		}); err != nil {
			log.Error(err, "Failed to update status after node failure handling")
			return ctrl.Result{}, err
		}

		// Requeue — next reconcile: hasNodeFailureEvent=false, PendingStartupResume=true,
		// shouldSkipOperation=true (Freeze blocked) → startup resume path runs Resume.
		log.Info("🔄 Node failure handled — requeuing to resume workloads on surviving nodes",
			"policy", policy.Name)
		return ctrl.Result{Requeue: true}, nil
	}

	// While a node failure is active AND the node is still NotReady, keep force-deleting
	// any pods that have since become Terminating.
	// Stop blocking (and allow manual operations like Freeze) once:
	//   (a) a new unhandled operationId arrives, OR
	//   (b) the failed node has recovered (no longer NotReady)
	if policy.Spec.HandleNodeFailure && policy.Status.FailedNodeName != "" {
		// Let manual operations (new operationId) override the cleanup loop
		hasNewManualOp := !r.shouldSkipOperation(&policy)
		if hasNewManualOp {
			// Fall through — new manual command takes precedence over cleanup loop
		} else {
			// Check if the node is still NotReady before continuing to loop
			nodeStillFailed := false
			nodeList := &corev1.NodeList{}
			if err := r.List(ctx, nodeList); err == nil {
				for i := range nodeList.Items {
					if nodeList.Items[i].Name == policy.Status.FailedNodeName {
						for _, cond := range nodeList.Items[i].Status.Conditions {
							if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
								nodeStillFailed = true
							}
						}
					}
				}
			}
			if nodeStillFailed {
				r.forceDeleteTerminatingPods(ctx, &policy, policy.Status.FailedNodeName)
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			// Node recovered — clear failedNodeName so we stop looping
			if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				latest := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
					return err
				}
				patchBase := latest.DeepCopy()
				latest.Status.FailedNodeName = ""
				return r.Status().Patch(ctx, latest, client.MergeFrom(patchBase))
			}); err != nil {
				log.Error(err, "Failed to clear FailedNodeName after node recovery")
			}
			if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
				return ctrl.Result{}, err
			}
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
			// DEFER balancing if resume is still in progress (or not yet started)
			// This prevents "mixing" rolling restarts with an active scale-up operation.
			if policy.Status.Phase != appsv1alpha1.PhaseResumed {
				log.Info("⏳ Deferring pod balancing: policy is not in Resumed phase",
					"currentPhase", policy.Status.Phase,
					"policy", policy.Name)
				// Return without marking NodeReadyEventHandledAt, so it gets processed
				// in the next reconciliation (e.g., when phase changes to Resumed).
				return ctrl.Result{}, nil
			}

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
			if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed, errMsg); err != nil {
				log.Error(err, "Failed to update status")
				return ctrl.Result{}, err
			}

			// Don't return error - namespace not existing is an expected state
			// Kubernetes will auto-reconcile when namespace is created
			return ctrl.Result{}, nil
		}
		// Other error (permissions, api server down, etc) - this should be retried
		log.Error(err, "Failed to get target namespace")

		if statusErr := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed, fmt.Sprintf("Failed to get namespace: %v", err)); statusErr != nil {
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

		// All pre-conditions passed - update status and let the standard startup resume flow take over
		log.Info("✅ All pre-conditions passed for background startup resume",
			"policy", policy.Name)

		if updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); getErr != nil {
				return getErr
			}
			now := metav1.Now()
			if latestPolicy.Status.PreConditionsStatus == nil {
				latestPolicy.Status.PreConditionsStatus = &appsv1alpha1.PreConditionsStatus{}
			}
			latestPolicy.Status.PreConditionsStatus.Checking = false
			latestPolicy.Status.PreConditionsStatus.Passed = true
			latestPolicy.Status.PreConditionsStatus.LastCheckedAt = &now
			latestPolicy.Status.PreConditionsStatus.Message = "All pre-conditions passed"

			// We DO NOT set PendingStartupResume = false here.
			// We want the other branch to pick up the actual resume operation.

			return r.Status().Update(ctx, latestPolicy)
		}); updateErr != nil {
			log.Error(updateErr, "Failed to update pre-conditions status")
		}

		// Requeue immediately to let the PendingStartupResume branch handle the resume
		return ctrl.Result{Requeue: true}, nil
	}

	log.Info("Preparing to execute operation", "action", policy.Spec.Action, "namespace", policy.Spec.TargetNamespace)

	// Update status to processing phase
	var phase appsv1alpha1.Phase
	if policy.Spec.Action == appsv1alpha1.LifecycleActionFreeze {
		phase = appsv1alpha1.PhaseFreezing
	} else {
		phase = appsv1alpha1.PhaseResuming
	}

	if err := r.updateStatus(ctx, &policy, phase, "Processing request"); err != nil {
		log.Error(err, "Failed to update status to processing")
		return ctrl.Result{}, err
	}

	// List Deployments in target namespace with selector
	deployments, err := r.listDeployments(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		log.Error(err, "Failed to list deployments")
		if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
			fmt.Sprintf("Failed to list deployments: %v", err)); err != nil {
			log.Error(err, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	// List StatefulSets in target namespace with selector
	statefulSets, err := r.listStatefulSets(ctx, policy.Spec.TargetNamespace, policy.Spec.Selector)
	if err != nil {
		log.Error(err, "Failed to list statefulsets")
		if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
			fmt.Sprintf("Failed to list statefulsets: %v", err)); err != nil {
			log.Error(err, "Failed to update status")
		}
		return ctrl.Result{}, err
	}

	log.Info("Found resources",
		"deployments", len(deployments.Items),
		"statefulsets", len(statefulSets.Items))

	// Check if no resources found (Initial check before filtering)
	if len(deployments.Items) == 0 && len(statefulSets.Items) == 0 {
		msg := "No deployments or statefulsets found in namespace"
		if policy.Spec.Selector != nil {
			msg = "No resources matched the selector in namespace"
		}
		log.Info(msg, "namespace", policy.Spec.TargetNamespace)

		phase := appsv1alpha1.PhaseFrozen
		if policy.Spec.Action == appsv1alpha1.LifecycleActionResume {
			phase = appsv1alpha1.PhaseResumed
		}
		if err := r.updateStatus(ctx, &policy, phase, msg); err != nil {
			log.Error(err, "Failed to update status")
		}
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
					fmt.Sprintf("Failed to freeze deployment %s: %v", deployment.Name, err)); err != nil {
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
					fmt.Sprintf("Failed to freeze statefulset %s: %v", sts.Name, err)); err != nil {
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
				len(deployments.Items), len(statefulSets.Items))); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}

	case appsv1alpha1.LifecycleActionResume:
		// FILTER: Only count and process workloads that were actually frozen
		deployments, statefulSets = r.filterWorkloadsRequiringResume(deployments, statefulSets)

		// Check if no workloads require resume after filtering
		if len(deployments.Items) == 0 && len(statefulSets.Items) == 0 {
			log.Info("No workloads require resume (none were frozen or all already resumed)",
				"namespace", policy.Spec.TargetNamespace)

			if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseResumed,
				"No workloads require resume (all already resumed)"); err != nil {
				log.Error(err, "Failed to update status")
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// Execute the resume operation

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
					fmt.Sprintf("Failed to resume namespace: %v", err)); err != nil {
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
						fmt.Sprintf("Failed to resume deployment %s: %v", deployment.Name, err)); err != nil {
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
						fmt.Sprintf("Failed to resume statefulset %s: %v", sts.Name, err)); err != nil {
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
		log.Info("⚠️  Node transitioned to NotReady — checking for handleNodeFailure policies",
			"node", node.Name)

		// List all policies with handleNodeFailure=true
		policyList := &appsv1alpha1.NamespaceLifecyclePolicyList{}
		if err := r.List(ctx, policyList); err != nil {
			log.Error(err, "Failed to list NamespaceLifecyclePolicy resources for node failure")
			return nil
		}

		now := metav1.Now()
		var requests []reconcile.Request
		var candidatePolicies []string

		for i := range policyList.Items {
			policy := &policyList.Items[i]
			if !policy.Spec.HandleNodeFailure {
				continue
			}

			// Idempotency: if this exact failure is already being processed, just re-enqueue
			alreadyTracking := policy.Status.FailedNodeName == node.Name &&
				policy.Status.NodeFailureEventDetectedAt != nil &&
				(policy.Status.NodeFailureEventHandledAt == nil ||
					policy.Status.NodeFailureEventDetectedAt.After(policy.Status.NodeFailureEventHandledAt.Time))
			if !alreadyTracking {
				policy.Status.FailedNodeName = node.Name
				policy.Status.NodeFailureEventDetectedAt = &now
				if err := r.Status().Update(ctx, policy); err != nil {
					log.Error(err, "Failed to update node failure status", "policy", policy.Name)
					continue
				}
			}

			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      policy.Name,
					Namespace: policy.Namespace,
				},
			})
			candidatePolicies = append(candidatePolicies, policy.Name)
		}

		if len(requests) > 0 {
			log.Info("Enqueuing policies for node failure handling",
				"node", node.Name,
				"policies", candidatePolicies)
		} else {
			log.V(1).Info("No handleNodeFailure policies found for NotReady node", "node", node.Name)
		}
		return requests
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

	log.Info("✅ Pod balancing completed — rolling restarts triggered",
		"policy", policy.Name,
		"deployments", len(deployments.Items),
		"statefulsets", len(statefulSets.Items))

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
					// Trigger if PendingFreeze changed from false to true
					if !oldPolicy.Status.PendingFreeze && newPolicy.Status.PendingFreeze {
						return true
					}

					// Trigger if PendingFreeze is true and the delay start time changed
					// This is crucial for operator restarts where ApplyStartupPolicy resets this timestamp
					if newPolicy.Status.PendingFreeze && newPolicy.Status.FreezeDelayStartedAt != nil {
						if oldPolicy.Status.FreezeDelayStartedAt == nil ||
							!newPolicy.Status.FreezeDelayStartedAt.Equal(oldPolicy.Status.FreezeDelayStartedAt) {
							return true
						}
					}

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

					// Trigger if a new node failure event was detected
					if newPolicy.Status.NodeFailureEventDetectedAt != nil {
						if oldPolicy.Status.NodeFailureEventDetectedAt == nil ||
							newPolicy.Status.NodeFailureEventDetectedAt.After(oldPolicy.Status.NodeFailureEventDetectedAt.Time) {
							return true
						}
					}
				}

				// Don't trigger on other status updates
				return false
			},
			CreateFunc: func(e event.CreateEvent) bool {
				// Reconcile on create events IF there is a pending startup resume, pending freeze,
				// or an unhandled node failure. This ensures that after operator restart, policies
				// already in one of these states are immediately enqueued for reconciliation.
				policy, ok := e.Object.(*appsv1alpha1.NamespaceLifecyclePolicy)
				if ok {
					if policy.Status.PendingStartupResume || policy.Status.PendingFreeze {
						return true
					}
					// Unhandled node failure event (detected but not yet processed)
					if policy.Spec.HandleNodeFailure &&
						policy.Status.NodeFailureEventDetectedAt != nil &&
						(policy.Status.NodeFailureEventHandledAt == nil ||
							policy.Status.NodeFailureEventDetectedAt.After(policy.Status.NodeFailureEventHandledAt.Time)) {
						return true
					}
					// Unhandled operationId — spec was changed before operator restarted
					if policy.Spec.OperationId != "" && policy.Status.LastHandledOperationId != policy.Spec.OperationId {
						return true
					}
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
func (r *NamespaceLifecyclePolicyReconciler) waitForPreConditions(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy) error {
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
				// Check if there's a NEW manual operation (operationId changed) with action=Freeze
				// Even during startup operations, a manual Freeze command should take precedence
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
					status.Message = "Pre-conditions check cancelled - new manual Freeze operation received"
					if updateErr := r.updatePreConditionsStatus(ctx, latestPolicy, status); updateErr != nil {
						log.Error(updateErr, "Failed to update pre-conditions status")
					}
					return fmt.Errorf("pre-conditions cancelled: new manual Freeze operation")
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

// filterWorkloadsRequiringResume filters deployments and statefulsets that have the original replicas annotation
func (r *NamespaceLifecyclePolicyReconciler) filterWorkloadsRequiringResume(
	deployments *appsv1.DeploymentList,
	statefulSets *appsv1.StatefulSetList,
) (*appsv1.DeploymentList, *appsv1.StatefulSetList) {
	filteredDeps := &appsv1.DeploymentList{Items: []appsv1.Deployment{}}
	for i := range deployments.Items {
		if _, exists := deployments.Items[i].Annotations[appsv1alpha1.AnnotationOriginalReplicas]; exists {
			filteredDeps.Items = append(filteredDeps.Items, deployments.Items[i])
		}
	}
	filteredSts := &appsv1.StatefulSetList{Items: []appsv1.StatefulSet{}}
	for i := range statefulSets.Items {
		if _, exists := statefulSets.Items[i].Annotations[appsv1alpha1.AnnotationOriginalReplicas]; exists {
			filteredSts.Items = append(filteredSts.Items, statefulSets.Items[i])
		}
	}
	return filteredDeps, filteredSts
}
