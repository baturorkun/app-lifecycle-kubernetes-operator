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
	"time"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// workloadItem represents a deployment or statefulset to resume
type workloadItem struct {
	IsDeployment bool
	Deployment   *appsv1.Deployment
	StatefulSet  *appsv1.StatefulSet
	Name         string
}

// resumeWithAdaptiveThrottling performs a throttled resume operation with signal monitoring
func (r *NamespaceLifecyclePolicyReconciler) resumeWithAdaptiveThrottling(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	deployments *appsv1.DeploymentList,
	statefulSets *appsv1.StatefulSetList,
	isStartupOperation bool,
) error {
	// Create a clean logger without framework metadata noise (controllerGroup, etc.)
	log := logf.Log.WithName("adaptive").WithValues("policy", policy.Name)
	ctx = logf.IntoContext(ctx, log)
	config := policy.Spec.AdaptiveThrottling

	// Combine all workloads into a single list
	allWorkloads := make([]workloadItem, 0, len(deployments.Items)+len(statefulSets.Items))
	for i := range deployments.Items {
		allWorkloads = append(allWorkloads, workloadItem{
			IsDeployment: true,
			Deployment:   &deployments.Items[i],
			Name:         deployments.Items[i].Name,
		})
	}
	for i := range statefulSets.Items {
		allWorkloads = append(allWorkloads, workloadItem{
			IsDeployment: false,
			StatefulSet:  &statefulSets.Items[i],
			Name:         statefulSets.Items[i].Name,
		})
	}

	totalWorkloads := int32(len(allWorkloads))
	if totalWorkloads == 0 {
		log.Info("No workloads to resume")
		return nil
	}

	// Initialize adaptive progress
	currentBatchSize := config.InitialBatchSize
	if currentBatchSize == 0 {
		currentBatchSize = 3 // default
	}
	minBatchSize := config.MinBatchSize
	if minBatchSize == 0 {
		minBatchSize = 1 // default
	}
	batchInterval := config.BatchInterval
	if batchInterval == 0 {
		batchInterval = 5 // default
	}

	resumedCount := int32(0)
	startTime := time.Now()

	log.Info("🚀 Starting adaptive throttling resume",
		"totalWorkloads", totalWorkloads,
		"initialBatchSize", currentBatchSize,
		"minBatchSize", minBatchSize,
		"batchInterval", batchInterval)

	// Process workloads in adaptive batches
	for resumedCount < totalWorkloads {
		// 0. Check for manual override/abort
		latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
		if err := r.Get(ctx, types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
			log.Error(err, "Failed to re-fetch policy to check for manual override")
		} else {
			// PRIORITY LOGIC:
			// 1. Is there a PENDING manual operation?
			isManualPending := latestPolicy.Spec.OperationId != "" &&
				latestPolicy.Status.LastHandledOperationId != latestPolicy.Spec.OperationId

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

			// 3. For Manual resumes: also abort if action changed to Freeze (even stale)
			// But for Startup resumes: ONLY abort if there's a pending action, not on stale freezes.
			// This allows startup policy to resume even when spec.action was left as Freeze from a previous manual op.
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
				return fmt.Errorf("operation aborted due to manual override")
			}
		}

		// 1. Collect signals (targetNamespace-scoped for container restarts)
		signals, metrics, err := r.collectSignals(ctx, config, policy.Spec.TargetNamespace)
		if err != nil {
			// If context was canceled (shutdown), stop immediately
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if config.FallbackOnMetricsUnavailable {
				log.Error(err, "Failed to collect signals, proceeding without throttling")
				signals = []appsv1alpha1.Signal{} // empty signals, proceed
			} else {
				return fmt.Errorf("failed to collect signals: %w", err)
			}
		}

		// Double check context after metrics collection
		if ctx.Err() != nil {
			return ctx.Err()
		}

		now := metav1.Now()

		// 2. Check for critical signals (Node NotReady)
		if hasSignal(signals, appsv1alpha1.SignalNodeNotReady) {
			log.Info("🔴 Node(s) NotReady detected, pausing resume",
				"nodesAvgCpu", fmt.Sprintf("%d%%", metrics.AvgCPUPercent),
				"nodesAvgMem", fmt.Sprintf("%d%%", metrics.AvgMemPercent),
				"pending", metrics.PendingPods,
				"crash", metrics.UnhealthyPods,
				"signals", getSignalTypes(signals))

			// Update status to show we're waiting
			if err := r.updateAdaptiveProgress(ctx, policy, resumedCount, totalWorkloads,
				currentBatchSize, signals, &now, "Paused - waiting for nodes to become ready"); err != nil {
				log.Error(err, "Failed to update adaptive progress")
			}

			// Wait for node(s) to become ready
			if !r.waitForSignalClear(ctx, config, policy, appsv1alpha1.SignalNodeNotReady, startTime) {
				return fmt.Errorf("timeout waiting for nodes to become ready")
			}

			// Continue to next iteration to re-evaluate signals
			continue
		}

		// 3. Calculate batch size based on signals
		previousBatchSize := currentBatchSize
		currentBatchSize = r.calculateBatchSize(signals, config, currentBatchSize)

		if currentBatchSize != previousBatchSize {
			actualInitial := config.InitialBatchSize
			if actualInitial == 0 {
				actualInitial = 3
			}

			// Add unique keyword for slowdown detection
			if currentBatchSize < previousBatchSize {
				// Extract NodeUsage signal details (which nodes exceeded threshold)
				highUsageNodes := []string{}
				for _, sig := range signals {
					if sig.Type == appsv1alpha1.SignalNodeUsage {
						highUsageNodes = append(highUsageNodes, sig.Message)
					}
				}

				logFields := []interface{}{
					"previous", previousBatchSize,
					"new", currentBatchSize,
					"initialTarget", actualInitial,
					"signals", getSignalTypes(signals),
					"nodesAvgCpu", fmt.Sprintf("%d%%", metrics.AvgCPUPercent),
					"nodesAvgMem", fmt.Sprintf("%d%%", metrics.AvgMemPercent),
				}

				if len(highUsageNodes) > 0 {
					logFields = append(logFields, "highUsageNodes", highUsageNodes)
				}

				log.Info("🐌 SLOWDOWN_DETECTED: Batch size reduced due to signals", logFields...)
			} else {
				log.Info("🐌 Throttling: Adjusted batch size",
					"previous", previousBatchSize,
					"new", currentBatchSize,
					"initialTarget", actualInitial,
					"signals", getSignalTypes(signals))
			}
		}

		// 4. Process next batch
		batchEnd := resumedCount + currentBatchSize
		if batchEnd > totalWorkloads {
			batchEnd = totalWorkloads
		}
		batch := allWorkloads[resumedCount:batchEnd]

		log.Info("⚙️ Processing batch",
			"batchSize", len(batch),
			"progress", fmt.Sprintf("%d/%d", resumedCount, totalWorkloads),
			"nodesAvgCpu", fmt.Sprintf("%d%%", metrics.AvgCPUPercent),
			"nodesAvgMem", fmt.Sprintf("%d%%", metrics.AvgMemPercent),
			"pending", metrics.PendingPods,
			"crash", metrics.UnhealthyPods,
			"signals", getSignalTypes(signals))

		// Update status before processing batch
		if err := r.updateAdaptiveProgress(ctx, policy, resumedCount, totalWorkloads,
			currentBatchSize, signals, &now,
			fmt.Sprintf("Resuming batch (batch size: %d, %d/%d completed)", len(batch), resumedCount, totalWorkloads)); err != nil {
			log.Error(err, "Failed to update adaptive progress")
		}

		// 5. Resume workloads in this batch
		for _, workload := range batch {
			progress := fmt.Sprintf("[%d/%d]", resumedCount+1, totalWorkloads)
			if workload.IsDeployment {
				log.Info("🚀 Resuming deployment",
					"progress", progress,
					"name", workload.Name,
					"batchSize", len(batch))
				if err := r.resumeDeployment(ctx, workload.Deployment); err != nil {
					return fmt.Errorf("failed to resume deployment %s: %w", workload.Name, err)
				}
			} else {
				log.Info("🚀 Resuming statefulset",
					"progress", progress,
					"name", workload.Name,
					"batchSize", len(batch))
				if err := r.resumeStatefulSet(ctx, workload.StatefulSet); err != nil {
					return fmt.Errorf("failed to resume statefulset %s: %w", workload.Name, err)
				}
			}
			resumedCount++
		}

		// 6. Wait between batches (unless this was the last batch)
		if resumedCount < totalWorkloads {
			waitDuration := r.calculateWaitDuration(signals, batchInterval)
			log.Info("💤 Waiting between batches",
				"duration", waitDuration,
				"nodesAvgCpu", fmt.Sprintf("%d%%", metrics.AvgCPUPercent),
				"nodesAvgMem", fmt.Sprintf("%d%%", metrics.AvgMemPercent),
				"pending", metrics.PendingPods,
				"crash", metrics.UnhealthyPods,
				"signals", getSignalTypes(signals))
			time.Sleep(waitDuration)
		}
	}

	// All workloads resumed successfully
	elapsed := time.Since(startTime)
	log.Info("✅ Adaptive throttling resume completed",
		"totalWorkloads", totalWorkloads,
		"duration", elapsed)

	// Final status update - show completion with clear messages
	finalCheckTime := metav1.Now()
	finalMessage := fmt.Sprintf("All workloads resumed successfully (%d/%d completed)", totalWorkloads, totalWorkloads)

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      policy.Name,
			Namespace: policy.Namespace,
		}, latestPolicy); err != nil {
			return err
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestPolicy.DeepCopy()

		// Update adaptive progress with final completion status
		latestPolicy.Status.AdaptiveProgress = &appsv1alpha1.AdaptiveProgressStatus{
			TotalWorkloads:   totalWorkloads,
			ResumedWorkloads: totalWorkloads, // All workloads completed
			CurrentBatchSize: currentBatchSize,
			ActiveSignals:    []appsv1alpha1.Signal{}, // No active signals
			LastCheckTime:    &finalCheckTime,
			Message:          finalMessage,
		}

		// Update root message to show completion
		latestPolicy.Status.Message = finalMessage

		log.V(1).Info("Updating final adaptive progress",
			"resumed", totalWorkloads,
			"total", totalWorkloads)

		// Use Patch for status to be resilient to concurrent updates
		return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
	}); err != nil {
		log.Error(err, "Failed to update final adaptive progress")
		// Don't return error - resume is complete
	}

	return nil
}

// calculateBatchSize determines the batch size based on active signals
func (r *NamespaceLifecyclePolicyReconciler) calculateBatchSize(
	signals []appsv1alpha1.Signal,
	config *appsv1alpha1.AdaptiveThrottlingConfig,
	currentBatchSize int32,
) int32 {
	initialBatchSize := config.InitialBatchSize
	if initialBatchSize == 0 {
		initialBatchSize = 3
	}
	minBatchSize := config.MinBatchSize
	if minBatchSize == 0 {
		minBatchSize = 1
	}

	// Critical signals should have been handled by waitForSignalClear
	// So we only handle Warning and Info severity signals here

	// Check for NodePressure (Warning severity)
	if hasSignal(signals, appsv1alpha1.SignalNodePressure) && config.SignalChecks.CheckNodePressure != nil {
		slowdownPercent := config.SignalChecks.CheckNodePressure.SlowdownPercent
		if slowdownPercent == 0 {
			slowdownPercent = 50 // default to 50%
		}
		newSize := (initialBatchSize * slowdownPercent) / 100
		if newSize < minBatchSize {
			newSize = minBatchSize
		}
		return newSize
	}

	// Check for NodeUsage (Warning severity - PROACTIVE)
	// This signal fires when node actual usage exceeds threshold (default 80%)
	// Uses real-time kubelet metrics, more proactive than NodePressure
	if hasSignal(signals, appsv1alpha1.SignalNodeUsage) && config.SignalChecks.CheckNodeUsage != nil {
		slowdownPercent := config.SignalChecks.CheckNodeUsage.SlowdownPercent
		if slowdownPercent == 0 {
			slowdownPercent = 60 // default to 60%
		}
		newSize := (initialBatchSize * slowdownPercent) / 100
		if newSize < minBatchSize {
			newSize = minBatchSize
		}
		return newSize
	}

	// Check for ContainerRestarts (Warning severity)
	if hasSignal(signals, appsv1alpha1.SignalContainerRestarts) && config.SignalChecks.CheckContainerRestarts != nil {
		slowdownPercent := config.SignalChecks.CheckContainerRestarts.SlowdownPercent
		if slowdownPercent == 0 {
			slowdownPercent = 50 // default to 50%
		}
		newSize := (initialBatchSize * slowdownPercent) / 100
		if newSize < minBatchSize {
			newSize = minBatchSize
		}
		return newSize
	}

	// Check for PendingPods (Info severity)
	if hasSignal(signals, appsv1alpha1.SignalPendingPods) && config.SignalChecks.CheckPendingPods != nil {
		slowdownPercent := config.SignalChecks.CheckPendingPods.SlowdownPercent
		if slowdownPercent == 0 {
			slowdownPercent = 70 // default to 70%
		}
		newSize := (currentBatchSize * slowdownPercent) / 100
		if newSize < minBatchSize {
			newSize = minBatchSize
		}
		return newSize
	}

	// No signals - gradually return to initial batch size
	if currentBatchSize < initialBatchSize {
		return min(currentBatchSize+1, initialBatchSize)
	}

	return currentBatchSize
}

// calculateWaitDuration determines how long to wait between batches based on signals
func (r *NamespaceLifecyclePolicyReconciler) calculateWaitDuration(
	signals []appsv1alpha1.Signal,
	baseDuration int32,
) time.Duration {
	duration := time.Duration(baseDuration) * time.Second

	// If there are pressure signals, wait longer
	if hasSignal(signals, appsv1alpha1.SignalNodePressure) {
		duration = duration * 2
	}

	// If there are pending pods, wait a bit longer
	if hasSignal(signals, appsv1alpha1.SignalPendingPods) {
		duration = duration * 3 / 2
	}

	// If containers are crashing, wait significantly longer to allow recovery
	if hasSignal(signals, appsv1alpha1.SignalContainerRestarts) {
		duration = duration * 2
	}

	return duration
}

// waitForSignalClear waits for a specific signal type to clear
func (r *NamespaceLifecyclePolicyReconciler) waitForSignalClear(
	ctx context.Context,
	config *appsv1alpha1.AdaptiveThrottlingConfig,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	signalType appsv1alpha1.SignalType,
	startTime time.Time,
) bool {
	log := ctrl.LoggerFrom(ctx)

	// Get wait configuration
	waitInterval := int32(20)  // default 20 seconds
	maxWaitTime := int32(1800) // default 30 minutes
	if config.SignalChecks.CheckNodeReady != nil {
		if config.SignalChecks.CheckNodeReady.WaitInterval > 0 {
			waitInterval = config.SignalChecks.CheckNodeReady.WaitInterval
		}
		if config.SignalChecks.CheckNodeReady.MaxWaitTime > 0 {
			maxWaitTime = config.SignalChecks.CheckNodeReady.MaxWaitTime
		}
	}

	elapsed := time.Since(startTime)
	ticker := time.NewTicker(time.Duration(waitInterval) * time.Second)
	defer ticker.Stop()

	for elapsed.Seconds() < float64(maxWaitTime) {
		select {
		case <-ctx.Done():
			log.Info("Context cancelled while waiting for signal to clear")
			return false
		case <-ticker.C:
			// Check if signal has cleared (targetNamespace-scoped for container restarts)
			signals, metrics, err := r.collectSignals(ctx, config, policy.Spec.TargetNamespace)
			if err != nil {
				if config.FallbackOnMetricsUnavailable {
					log.Error(err, "Failed to collect signals during wait, assuming cleared")
					return true
				}
				log.Error(err, "Failed to collect signals during wait")
				return false
			}

			if !hasSignal(signals, signalType) {
				log.Info("Signal cleared",
					"signal", signalType,
					"nodesAvgCpu", fmt.Sprintf("%d%%", metrics.AvgCPUPercent),
					"nodesAvgMem", fmt.Sprintf("%d%%", metrics.AvgMemPercent),
					"pending", metrics.PendingPods,
					"crash", metrics.UnhealthyPods,
					"waitedSeconds", int(elapsed.Seconds()))
				return true
			}

			elapsed = time.Since(startTime)
			log.Info("Still waiting for signal to clear",
				"signal", signalType,
				"elapsedSeconds", int(elapsed.Seconds()),
				"maxWaitSeconds", maxWaitTime)
		}
	}

	log.Info("Timeout waiting for signal to clear",
		"signal", signalType,
		"waitedSeconds", int(elapsed.Seconds()))
	return false
}

// updateAdaptiveProgress updates the adaptive progress status
func (r *NamespaceLifecyclePolicyReconciler) updateAdaptiveProgress(
	ctx context.Context,
	policy *appsv1alpha1.NamespaceLifecyclePolicy,
	resumedWorkloads int32,
	totalWorkloads int32,
	currentBatchSize int32,
	signals []appsv1alpha1.Signal,
	checkTime *metav1.Time,
	message string,
) error {
	log := ctrl.LoggerFrom(ctx)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch latest version
		latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      policy.Name,
			Namespace: policy.Namespace,
		}, latestPolicy); err != nil {
			return err
		}

		// Create a patch helper based on the current state BEFORE our changes
		patchBase := latestPolicy.DeepCopy()

		// Update adaptive progress
		latestPolicy.Status.AdaptiveProgress = &appsv1alpha1.AdaptiveProgressStatus{
			TotalWorkloads:   totalWorkloads,
			ResumedWorkloads: resumedWorkloads,
			CurrentBatchSize: currentBatchSize,
			ActiveSignals:    signals,
			LastCheckTime:    checkTime,
			Message:          message,
		}

		// Update phase and message
		latestPolicy.Status.Phase = appsv1alpha1.PhaseResuming
		latestPolicy.Status.Message = message

		log.V(1).Info("Updating adaptive progress",
			"resumed", resumedWorkloads,
			"total", totalWorkloads,
			"batchSize", currentBatchSize,
			"signals", len(signals))

		// Use Patch for status to be resilient to concurrent updates
		return r.Status().Patch(ctx, latestPolicy, client.MergeFrom(patchBase))
	})
}

// getSignalTypes returns a list of active signal types as strings
func getSignalTypes(signals []appsv1alpha1.Signal) []string {
	types := make([]string, 0, len(signals))
	seen := make(map[string]bool)
	for _, s := range signals {
		typeName := string(s.Type)
		if !seen[typeName] {
			types = append(types, typeName)
			seen[typeName] = true
		}
	}
	return types
}

func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
