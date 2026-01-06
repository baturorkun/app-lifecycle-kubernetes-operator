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
) error {
	log := ctrl.LoggerFrom(ctx)
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
		// 1. Collect signals (cluster-wide)
		signals, err := r.collectSignals(ctx, config)
		if err != nil {
			if config.FallbackOnMetricsUnavailable {
				log.Error(err, "Failed to collect signals, proceeding without throttling")
				signals = []appsv1alpha1.Signal{} // empty signals, proceed
			} else {
				return fmt.Errorf("failed to collect signals: %w", err)
			}
		}

		now := metav1.Now()

		// 2. Check for critical signals (Node NotReady)
		if hasSignal(signals, appsv1alpha1.SignalNodeNotReady) {
			log.Info("🔴 Node(s) NotReady detected, pausing resume",
				"activeSignals", len(signals))

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
			log.Info("🐌 Throttling: Adjusted batch size",
				"from", previousBatchSize,
				"to", currentBatchSize,
				"activeSignals", len(signals))
		}

		// 4. Process next batch
		batchEnd := resumedCount + currentBatchSize
		if batchEnd > totalWorkloads {
			batchEnd = totalWorkloads
		}
		batch := allWorkloads[resumedCount:batchEnd]

		log.Info("📊 Processing batch",
			"batchSize", len(batch),
			"progress", fmt.Sprintf("%d/%d", resumedCount, totalWorkloads),
			"activeSignals", len(signals))

		// Update status before processing batch
		if err := r.updateAdaptiveProgress(ctx, policy, resumedCount, totalWorkloads,
			currentBatchSize, signals, &now,
			fmt.Sprintf("Resuming batch (batch size: %d, %d/%d completed)", len(batch), resumedCount, totalWorkloads)); err != nil {
			log.Error(err, "Failed to update adaptive progress")
		}

		// 5. Resume workloads in this batch
		for _, workload := range batch {
			if workload.IsDeployment {
				log.Info("Resuming deployment", "name", workload.Name)
				if err := r.resumeDeployment(ctx, workload.Deployment); err != nil {
					return fmt.Errorf("failed to resume deployment %s: %w", workload.Name, err)
				}
			} else {
				log.Info("Resuming statefulset", "name", workload.Name)
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
				"activeSignals", len(signals))
			time.Sleep(waitDuration)
		}
	}

	// All workloads resumed successfully
	elapsed := time.Since(startTime)
	log.Info("✅ Adaptive throttling resume completed",
		"totalWorkloads", totalWorkloads,
		"duration", elapsed)

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
			// Check if signal has cleared (cluster-wide)
			signals, err := r.collectSignals(ctx, config)
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

		return r.Status().Update(ctx, latestPolicy)
	})
}

// min returns the minimum of two int32 values
func min(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}
