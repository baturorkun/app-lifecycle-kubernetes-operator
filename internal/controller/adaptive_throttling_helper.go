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

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// checkManualOverride checks if there is a pending manual operation that should abort the current resume
func (r *NamespaceLifecyclePolicyReconciler) checkManualOverride(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	isStartupOperation bool,
) (bool, error) {
	log := logf.FromContext(ctx)

	latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
	if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
		return false, fmt.Errorf("failed to re-fetch policy: %w", err)
	}

	// PRIORITY LOGIC:
	// 1. Is there a PENDING manual operation?
	isManualPending := latestPolicy.Spec.OperationId != "" &&
		latestPolicy.Status.LastHandledOperationId != latestPolicy.Spec.OperationId

	// SAFETY CHECK: If this is a startup operation and LastHandledOperationId is empty,
	// this "new" operation is likely just a stale one from before status was created/reset.
	// We should ignore it to allow startup policy to proceed.
	if isStartupOperation && latestPolicy.Status.LastHandledOperationId == "" {
		isManualPending = false
	}

	// 2. Should we abort?
	// - If this is a Startup Resume, ANY pending manual action takes priority.
	// - If this is a Manual Resume, only a DIFFERENT manual action (new OpId) takes priority.
	shouldAbort := false
	reason := ""

	if isManualPending {
		if isStartupOperation {
			shouldAbort = true
			reason = "Manual command takes precedence over Startup Resume"
		} else if latestPolicy.Spec.OperationId != policy.Spec.OperationId {
			shouldAbort = true
			reason = "Newer manual command detected"
		}
	}

	// 3. For Manual resumes: also abort if action changed to Freeze
	// But for Startup resumes: ONLY abort if there's a pending action, not on stale freezes.
	if !isStartupOperation && latestPolicy.Spec.Action == appsv1alpha1.LifecycleActionFreeze && !shouldAbort {
		shouldAbort = true
		reason = "Action is now Freeze"
	}

	if shouldAbort {
		log.Info("🛑 Manual override detected - aborting active resume process",
			"reason", reason,
			"newAction", latestPolicy.Spec.Action,
			"newOperationId", latestPolicy.Spec.OperationId,
			"isStartup", isStartupOperation)
		return true, nil
	}

	return false, nil
}
