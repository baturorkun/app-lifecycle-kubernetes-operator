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
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=apps.ops.dev,resources=namespacelifecyclepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps.ops.dev,resources=namespacelifecyclepolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps.ops.dev,resources=namespacelifecyclepolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=core,resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch

// shouldSkipOperation checks if the operation should be skipped based on operationId
func (r *NamespaceLifecyclePolicyReconciler) shouldSkipOperation(policy *appsv1alpha1.NamespaceLifecyclePolicy) bool {
	// If no operationId specified, always process
	if policy.Spec.OperationId == "" {
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
func (r *NamespaceLifecyclePolicyReconciler) freezeDeployment(ctx context.Context, deployment *appsv1.Deployment) error {
	// If already frozen (replicas = 0), skip
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		return nil
	}

	// Store original replica count in annotation
	if deployment.Annotations == nil {
		deployment.Annotations = make(map[string]string)
	}

	originalReplicas := int32(1) // default
	if deployment.Spec.Replicas != nil {
		originalReplicas = *deployment.Spec.Replicas
	}

	deployment.Annotations[appsv1alpha1.AnnotationOriginalReplicas] = strconv.Itoa(int(originalReplicas))

	// Set replicas to 0
	zero := int32(0)
	deployment.Spec.Replicas = &zero

	return r.Update(ctx, deployment)
}

// freezeStatefulSet sets the statefulset replicas to 0 and stores the original count in an annotation
func (r *NamespaceLifecyclePolicyReconciler) freezeStatefulSet(ctx context.Context, sts *appsv1.StatefulSet) error {
	// If already frozen (replicas = 0), skip
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
		return nil
	}

	// Store original replica count in annotation
	if sts.Annotations == nil {
		sts.Annotations = make(map[string]string)
	}

	originalReplicas := int32(1) // default
	if sts.Spec.Replicas != nil {
		originalReplicas = *sts.Spec.Replicas
	}

	sts.Annotations[appsv1alpha1.AnnotationOriginalReplicas] = strconv.Itoa(int(originalReplicas))

	// Set replicas to 0
	zero := int32(0)
	sts.Spec.Replicas = &zero

	return r.Update(ctx, sts)
}

// resumeDeployment restores the deployment replicas from the annotation
func (r *NamespaceLifecyclePolicyReconciler) resumeDeployment(ctx context.Context, deployment *appsv1.Deployment) error {
	log := logf.FromContext(ctx)

	// Check if there's a stored original replica count
	originalReplicasStr, exists := deployment.Annotations[appsv1alpha1.AnnotationOriginalReplicas]
	if !exists {
		log.Info("Skipping resume: deployment was not frozen",
			"deployment", deployment.Name,
			"namespace", deployment.Namespace,
			"reason", "Missing freeze annotation")
		return nil
	}

	originalReplicas, err := strconv.Atoi(originalReplicasStr)
	if err != nil {
		return err
	}

	// Restore original replica count
	replicas := int32(originalReplicas)
	deployment.Spec.Replicas = &replicas

	// Remove the annotation
	delete(deployment.Annotations, appsv1alpha1.AnnotationOriginalReplicas)

	return r.Update(ctx, deployment)
}

// resumeStatefulSet restores the statefulset replicas from the annotation
func (r *NamespaceLifecyclePolicyReconciler) resumeStatefulSet(ctx context.Context, sts *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx)

	// Check if there's a stored original replica count
	originalReplicasStr, exists := sts.Annotations[appsv1alpha1.AnnotationOriginalReplicas]
	if !exists {
		log.Info("Skipping resume: statefulset was not frozen",
			"statefulset", sts.Name,
			"namespace", sts.Namespace,
			"reason", "Missing freeze annotation")
		return nil
	}

	originalReplicas, err := strconv.Atoi(originalReplicasStr)
	if err != nil {
		return err
	}

	// Restore original replica count
	replicas := int32(originalReplicas)
	sts.Spec.Replicas = &replicas

	// Remove the annotation
	delete(sts.Annotations, appsv1alpha1.AnnotationOriginalReplicas)

	return r.Update(ctx, sts)
}

// updateStatus updates the policy status with phase, message and lastHandledOperationId
func (r *NamespaceLifecyclePolicyReconciler) updateStatus(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy, phase appsv1alpha1.Phase, message string) error {
	policy.Status.Phase = phase
	policy.Status.Message = message
	policy.Status.LastHandledOperationId = policy.Spec.OperationId

	return r.Status().Update(ctx, policy)
}

// ApplyStartupPolicy applies the startup policy action to the namespace
// This is called once during operator startup for each policy
func (r *NamespaceLifecyclePolicyReconciler) ApplyStartupPolicy(ctx context.Context, policy *appsv1alpha1.NamespaceLifecyclePolicy) error {
	log := logf.FromContext(ctx)

	// Record timestamp - set this at the very beginning
	now := metav1.Now()
	policy.Status.LastStartupAt = &now

	// Skip if startup policy is Ignore
	if policy.Spec.StartupPolicy == appsv1alpha1.StartupPolicyIgnore {
		policy.Status.LastStartupAction = "SKIPPED_IGNORE"
		r.Status().Update(ctx, policy)
		log.Info("Startup policy check: no action needed",
			"policy", policy.Name,
			"startupPolicy", "Ignore",
			"reason", "Policy is set to Ignore")
		return nil
	}

	// Determine desired phase based on startup policy
	var desiredPhase appsv1alpha1.Phase
	var action appsv1alpha1.LifecycleAction
	if policy.Spec.StartupPolicy == appsv1alpha1.StartupPolicyFreeze {
		desiredPhase = appsv1alpha1.PhaseFrozen
		action = appsv1alpha1.LifecycleActionFreeze
	} else if policy.Spec.StartupPolicy == appsv1alpha1.StartupPolicyResume {
		desiredPhase = appsv1alpha1.PhaseResumed
		action = appsv1alpha1.LifecycleActionResume
	} else {
		policy.Status.LastStartupAction = "SKIPPED_UNKNOWN_POLICY"
		r.Status().Update(ctx, policy)
		log.Info("Startup policy check: no action needed",
			"policy", policy.Name,
			"startupPolicy", policy.Spec.StartupPolicy,
			"reason", "Unknown startup policy value")
		return nil
	}

	// Check if already in desired phase
	if policy.Status.Phase == desiredPhase {
		if desiredPhase == appsv1alpha1.PhaseFrozen {
			policy.Status.LastStartupAction = "NO_ACTION_ALREADY_FROZEN"
		} else {
			policy.Status.LastStartupAction = "NO_ACTION_ALREADY_RESUMED"
		}
		r.Status().Update(ctx, policy)
		log.Info("Startup policy check: no action needed",
			"policy", policy.Name,
			"startupPolicy", policy.Spec.StartupPolicy,
			"currentPhase", policy.Status.Phase,
			"reason", "Already in desired state")
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
			r.Status().Update(ctx, policy)
			log.Info("Startup policy check: no action needed",
				"policy", policy.Name,
				"targetNamespace", policy.Spec.TargetNamespace,
				"reason", "Target namespace not found")
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
	if action == appsv1alpha1.LifecycleActionFreeze {
		for i := range deployments.Items {
			deployment := &deployments.Items[i]
			if err := r.freezeDeployment(ctx, deployment); err != nil {
				log.Error(err, "Failed to freeze deployment during startup", "name", deployment.Name)
			}
		}
		for i := range statefulSets.Items {
			sts := &statefulSets.Items[i]
			if err := r.freezeStatefulSet(ctx, sts); err != nil {
				log.Error(err, "Failed to freeze statefulset during startup", "name", sts.Name)
			}
		}
		policy.Status.LastStartupAction = "FREEZE_APPLIED"
		log.Info("Startup policy applied: frozen", "policy", policy.Name)
	} else if action == appsv1alpha1.LifecycleActionResume {
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
		policy.Status.LastStartupAction = "RESUME_APPLIED"
		log.Info("Startup policy applied: resumed", "policy", policy.Name)
	}

	// Update status after applying
	return r.Status().Update(ctx, policy)
}

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// This implementation handles freezing/resuming Deployments and StatefulSets
// in a target namespace based on the policy configuration.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/reconcile
func (r *NamespaceLifecyclePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the NamespaceLifecyclePolicy CR
	var policy appsv1alpha1.NamespaceLifecyclePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if errors.IsNotFound(err) {
			log.Info("NamespaceLifecyclePolicy deleted", "name", req.Name)
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get NamespaceLifecyclePolicy")
		return ctrl.Result{}, err
	}

	// Check if this operation was already handled
	if r.shouldSkipOperation(&policy) {
		// Only check balancing if this reconcile was triggered by a node event
		if _, hasNodeEvent := policy.Annotations["apps.ops.dev/node-ready-event"]; hasNodeEvent {
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

			// Remove the node-ready annotation after processing
			delete(policy.Annotations, "apps.ops.dev/node-ready-event")
			if err := r.Update(ctx, &policy); err != nil {
				log.Error(err, "Failed to remove node-ready annotation")
				return ctrl.Result{}, err
			}
		}
		// If no node event annotation, silently skip balancing check

		return ctrl.Result{}, nil
	}

	// Check if target namespace exists
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.TargetNamespace}, namespace); err != nil {
		if errors.IsNotFound(err) {
			errMsg := fmt.Sprintf("Target namespace '%s' not found", policy.Spec.TargetNamespace)
			log.Info(errMsg)

			// Update status without setting lastHandledOperationId (allow retry when namespace is created)
			policy.Status.Phase = appsv1alpha1.PhaseFailed
			policy.Status.Message = errMsg
			// DO NOT set LastHandledOperationId - we want to retry when namespace is created
			if err := r.Status().Update(ctx, &policy); err != nil {
				log.Error(err, "Failed to update status")
				return ctrl.Result{}, err
			}

			// Don't return error - namespace not existing is an expected state
			// Kubernetes will auto-reconcile when namespace is created
			return ctrl.Result{}, nil
		}
		// Other error (permissions, api server down, etc) - this should be retried
		log.Error(err, "Failed to get target namespace")

		policy.Status.Phase = appsv1alpha1.PhaseFailed
		policy.Status.Message = fmt.Sprintf("Failed to get namespace: %v", err)
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			log.Error(statusErr, "Failed to update status")
		}

		return ctrl.Result{}, err
	}

	log.Info("Target namespace exists", "namespace", policy.Spec.TargetNamespace)

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
		if err := r.updateStatus(ctx, &policy, phase, msg); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}

		log.Info("No action taken, no resources found", "action", policy.Spec.Action)
		return ctrl.Result{}, nil
	}

	// Apply action
	if policy.Spec.Action == appsv1alpha1.LifecycleActionFreeze {
		// Freeze all deployments
		for i := range deployments.Items {
			deployment := &deployments.Items[i]
			log.Info("Freezing deployment", "name", deployment.Name)
			if err := r.freezeDeployment(ctx, deployment); err != nil {
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
			if err := r.freezeStatefulSet(ctx, sts); err != nil {
				log.Error(err, "Failed to freeze statefulset", "name", sts.Name)
				if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFailed,
					fmt.Sprintf("Failed to freeze statefulset %s: %v", sts.Name, err)); err != nil {
					log.Error(err, "Failed to update status")
				}
				return ctrl.Result{}, err
			}
		}

		// Update status to frozen
		if err := r.updateStatus(ctx, &policy, appsv1alpha1.PhaseFrozen,
			fmt.Sprintf("Successfully froze %d deployments and %d statefulsets",
				len(deployments.Items), len(statefulSets.Items))); err != nil {
			log.Error(err, "Failed to update status")
			return ctrl.Result{}, err
		}

	} else if policy.Spec.Action == appsv1alpha1.LifecycleActionResume {
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

		// Update status to resumed
		policy.Status.Phase = appsv1alpha1.PhaseResumed
		policy.Status.Message = fmt.Sprintf("Successfully resumed %d deployments and %d statefulsets",
			len(deployments.Items), len(statefulSets.Items))
		policy.Status.LastHandledOperationId = policy.Spec.OperationId

		// Set LastResumeAt timestamp for pod balancing
		now := metav1.Now()
		policy.Status.LastResumeAt = &now

		if err := r.Status().Update(ctx, &policy); err != nil {
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

// mapNodeReadyToPolicy maps Node Ready/NotReady events to NamespaceLifecyclePolicy resources
func (r *NamespaceLifecyclePolicyReconciler) mapNodeReadyToPolicy(ctx context.Context, obj client.Object) []reconcile.Request {
	log := logf.FromContext(ctx)

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

	// If node became NotReady, log and return - no reconciliation needed
	if !nodeReady {
		log.Info("⚠️  Node NotReady",
			"node", node.Name,
			"action", "Will trigger pod balancing when node becomes Ready")
		return nil
	}

	// Node is Ready - check if this is a recent transition
	if !isRecentTransition {
		// Skip if not a recent transition (likely resync/restart)
		return nil
	}

	// Log the Ready transition
	log.Info("🟢 Node Ready",
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
			// Add annotation to mark this reconcile was triggered by node event
			if policy.Annotations == nil {
				policy.Annotations = make(map[string]string)
			}
			policy.Annotations["apps.ops.dev/node-ready-event"] = time.Now().Format(time.RFC3339)

			if err := r.Update(ctx, policy); err != nil {
				log.Error(err, "Failed to add node-ready annotation", "policy", policy.Name)
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
	log := logf.FromContext(ctx)

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

	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}

	deployment.Spec.Template.Annotations["apps.ops.dev/restart-timestamp"] = time.Now().Format(time.RFC3339)

	log.Info("Triggering rolling restart for balanced pod distribution",
		"deployment", deployment.Name,
		"namespace", deployment.Namespace)

	return r.Update(ctx, deployment)
}

// triggerRollingRestartSts updates statefulset pod template annotation to trigger rolling update
func (r *NamespaceLifecyclePolicyReconciler) triggerRollingRestartSts(ctx context.Context, sts *appsv1.StatefulSet) error {
	log := logf.FromContext(ctx)

	if sts.Spec.Template.Annotations == nil {
		sts.Spec.Template.Annotations = make(map[string]string)
	}

	sts.Spec.Template.Annotations["apps.ops.dev/restart-timestamp"] = time.Now().Format(time.RFC3339)

	log.Info("Triggering rolling restart for balanced pod distribution",
		"statefulset", sts.Name,
		"namespace", sts.Namespace)

	return r.Update(ctx, sts)
}

// SetupWithManager sets up the controller with the Manager.
func (r *NamespaceLifecyclePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1alpha1.NamespaceLifecyclePolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
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
