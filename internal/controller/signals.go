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
	"encoding/json"
	"fmt"
	"strings"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeletStatsResponse represents the response from kubelet /stats/summary API
type KubeletStatsResponse struct {
	Node struct {
		CPU struct {
			UsageNanoCores uint64 `json:"usageNanoCores"`
		} `json:"cpu"`
		Memory struct {
			UsageBytes uint64 `json:"usageBytes"`
		} `json:"memory"`
	} `json:"node"`
}

// collectSignals gathers all active signals from node conditions and pending pods
// All signals are cluster-wide for proper coordination across multiple policies
func (r *NamespaceLifecyclePolicyReconciler) collectSignals(
	ctx context.Context,
	config *appsv1alpha1.AdaptiveThrottlingConfig,
) ([]appsv1alpha1.Signal, error) {
	signals := []appsv1alpha1.Signal{}

	if config == nil || config.SignalChecks == nil {
		return signals, nil
	}

	// Sinyal 1: Node Ready kontrolü
	if config.SignalChecks.CheckNodeReady != nil && config.SignalChecks.CheckNodeReady.Enabled {
		nodeReadySignals, err := r.checkNodeReadiness(ctx, config.NodeSelector)
		if err != nil {
			return signals, fmt.Errorf("failed to check node readiness: %w", err)
		}
		signals = append(signals, nodeReadySignals...)
	}

	// Sinyal 2: Node Pressure kontrolü
	if config.SignalChecks.CheckNodePressure != nil && config.SignalChecks.CheckNodePressure.Enabled {
		pressureSignals, err := r.checkNodePressure(ctx, config.NodeSelector, config.SignalChecks.CheckNodePressure.PressureTypes)
		if err != nil {
			return signals, fmt.Errorf("failed to check node pressure: %w", err)
		}
		signals = append(signals, pressureSignals...)
	}

	// Sinyal 3: Pending Pods kontrolü (cluster-wide)
	if config.SignalChecks.CheckPendingPods != nil && config.SignalChecks.CheckPendingPods.Enabled {
		pendingSignal, err := r.checkPendingPods(ctx, config.SignalChecks.CheckPendingPods.Threshold)
		if err != nil {
			return signals, fmt.Errorf("failed to check pending pods: %w", err)
		}
		if pendingSignal != nil {
			signals = append(signals, *pendingSignal)
		}
	}

	// Sinyal 4: Node Usage kontrolü (PROACTIVE - real-time kubelet metrics)
	if config.SignalChecks.CheckNodeUsage != nil && config.SignalChecks.CheckNodeUsage.Enabled {
		usageSignals, err := r.checkNodeUsage(
			ctx,
			config.NodeSelector,
			config.SignalChecks.CheckNodeUsage.CPUThresholdPercent,
			config.SignalChecks.CheckNodeUsage.MemoryThresholdPercent,
		)
		if err != nil {
			return signals, fmt.Errorf("failed to check node usage: %w", err)
		}
		signals = append(signals, usageSignals...)
	}

	return signals, nil
}

// checkNodeReadiness checks if any nodes are NotReady
func (r *NamespaceLifecyclePolicyReconciler) checkNodeReadiness(
	ctx context.Context,
	nodeSelector map[string]string,
) ([]appsv1alpha1.Signal, error) {
	signals := []appsv1alpha1.Signal{}

	// Default node selector to worker nodes if not specified
	if nodeSelector == nil {
		nodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
	}

	// List nodes matching selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels(nodeSelector)); err != nil {
		return signals, err
	}

	// Check each node's Ready condition
	for _, node := range nodeList.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == corev1.NodeReady {
				if condition.Status == corev1.ConditionTrue {
					ready = true
				}
				break
			}
		}

		if !ready {
			signals = append(signals, appsv1alpha1.Signal{
				Type:     appsv1alpha1.SignalNodeNotReady,
				Severity: appsv1alpha1.SignalSeverityCritical,
				Node:     node.Name,
				Message:  fmt.Sprintf("Node %s is NotReady", node.Name),
			})
		}
	}

	return signals, nil
}

// checkNodePressure checks for pressure conditions on nodes
func (r *NamespaceLifecyclePolicyReconciler) checkNodePressure(
	ctx context.Context,
	nodeSelector map[string]string,
	pressureTypes []string,
) ([]appsv1alpha1.Signal, error) {
	signals := []appsv1alpha1.Signal{}

	// Default pressure types if not specified
	if len(pressureTypes) == 0 {
		pressureTypes = []string{"MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"}
	}

	// Default node selector to worker nodes if not specified
	if nodeSelector == nil {
		nodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
	}

	// List nodes matching selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels(nodeSelector)); err != nil {
		return signals, err
	}

	// Check each node for pressure conditions
	for _, node := range nodeList.Items {
		for _, condition := range node.Status.Conditions {
			// Check if this condition type is in our pressure types list
			conditionTypeStr := string(condition.Type)
			for _, pressureType := range pressureTypes {
				if conditionTypeStr == pressureType && condition.Status == corev1.ConditionTrue {
					signals = append(signals, appsv1alpha1.Signal{
						Type:      appsv1alpha1.SignalNodePressure,
						Severity:  appsv1alpha1.SignalSeverityWarning,
						Node:      node.Name,
						Condition: pressureType,
						Message:   fmt.Sprintf("Node %s has %s", node.Name, pressureType),
					})
				}
			}
		}
	}

	return signals, nil
}

// checkPendingPods checks if there are too many pending pods in the cluster
// This function ONLY counts pods pending due to RESOURCE CONSTRAINTS (CPU/Memory/Pods)
// Other pending reasons (ContainerCreating, ImagePullBackOff, etc.) are NOT counted
// CLUSTER-WIDE: Counts pending pods across ALL namespaces to coordinate multiple policies
func (r *NamespaceLifecyclePolicyReconciler) checkPendingPods(
	ctx context.Context,
	threshold int32,
) (*appsv1alpha1.Signal, error) {
	// List ALL pods in the cluster (cluster-wide)
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return nil, err
	}

	// Count ONLY pods pending due to resource insufficiency
	pendingCount := int32(0)
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodPending {
			// Check if pod is unschedulable DUE TO RESOURCE CONSTRAINTS
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled &&
					condition.Status == corev1.ConditionFalse &&
					condition.Reason == corev1.PodReasonUnschedulable {
					// Check if the message indicates resource insufficiency
					// This filters out other Unschedulable reasons (taints, affinity, etc.)
					if strings.Contains(condition.Message, "Insufficient cpu") ||
						strings.Contains(condition.Message, "Insufficient memory") ||
						strings.Contains(condition.Message, "Insufficient pods") {
						pendingCount++
						break
					}
				}
			}
		}
	}

	// Check if we exceed threshold
	if pendingCount > threshold {
		return &appsv1alpha1.Signal{
			Type:     appsv1alpha1.SignalPendingPods,
			Severity: appsv1alpha1.SignalSeverityInfo,
			Count:    pendingCount,
			Message:  fmt.Sprintf("%d pods are pending cluster-wide due to resource constraints (threshold: %d)", pendingCount, threshold),
		}, nil
	}

	return nil, nil
}

// checkNodeUsage checks real-time CPU/Memory usage from kubelet
// This uses kubelet stats API to get ACTUAL usage (not requests/limits)
// Queries: /api/v1/nodes/{node}/proxy/stats/summary via Kubernetes API server
// Data lag: ~10 seconds (kubelet scrape interval)
func (r *NamespaceLifecyclePolicyReconciler) checkNodeUsage(
	ctx context.Context,
	nodeSelector map[string]string,
	cpuThreshold int32,
	memoryThreshold int32,
) ([]appsv1alpha1.Signal, error) {
	signals := []appsv1alpha1.Signal{}

	// Default thresholds if not specified
	if cpuThreshold == 0 {
		cpuThreshold = 80
	}
	if memoryThreshold == 0 {
		memoryThreshold = 80
	}

	// Default node selector to worker nodes if not specified
	if nodeSelector == nil {
		nodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
	}

	// List ALL nodes matching selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels(nodeSelector)); err != nil {
		return signals, err
	}

	// Check EACH NODE separately
	for _, node := range nodeList.Items {
		// Get node allocatable resources
		allocatableCPU := node.Status.Allocatable.Cpu()
		allocatableMemory := node.Status.Allocatable.Memory()

		if allocatableCPU.IsZero() || allocatableMemory.IsZero() {
			continue // Skip if resources not available
		}

		// Query kubelet for REAL-TIME CPU usage
		usedCPU, err := r.getNodeCPUUsage(ctx, node.Name)
		if err != nil {
			// If kubelet stats unavailable, skip this node
			// Log error but don't fail the entire signal check
			continue
		}

		// Query kubelet for REAL-TIME memory usage
		usedMemory, err := r.getNodeMemoryUsage(ctx, node.Name)
		if err != nil {
			continue
		}

		// Calculate REAL usage percentages
		cpuPercent := int32((usedCPU * 100) / allocatableCPU.MilliValue())
		memoryPercent := int32((usedMemory * 100) / allocatableMemory.Value())

		// Check if THIS NODE exceeds threshold
		if cpuPercent >= cpuThreshold || memoryPercent >= memoryThreshold {
			signals = append(signals, appsv1alpha1.Signal{
				Type:     appsv1alpha1.SignalNodeUsage,
				Severity: appsv1alpha1.SignalSeverityWarning,
				Node:     node.Name,
				Message:  fmt.Sprintf("Node %s high usage (CPU: %d%%, Memory: %d%%)", node.Name, cpuPercent, memoryPercent),
			})
		}
	}

	// ANY node exceeding threshold returns signals
	// Example: 3 nodes, 1 at 85% actual CPU usage → returns 1 signal
	return signals, nil
}

// getNodeCPUUsage queries kubelet stats API to get real-time CPU usage
// Returns CPU usage in millicores (e.g., 1500 = 1.5 CPU cores)
func (r *NamespaceLifecyclePolicyReconciler) getNodeCPUUsage(
	ctx context.Context,
	nodeName string,
) (int64, error) {
	// Use Kubernetes API server proxy to kubelet
	// This is more secure than direct kubelet access
	if r.RESTClient == nil {
		return 0, fmt.Errorf("REST client not available")
	}

	// Call kubelet stats/summary endpoint via API server proxy
	// Format: /api/v1/nodes/{node}/proxy/stats/summary
	result := r.RESTClient.Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy", "stats", "summary").
		Do(ctx)

	rawBody, err := result.Raw()
	if err != nil {
		return 0, fmt.Errorf("failed to query kubelet stats for node %s: %w", nodeName, err)
	}

	// Parse JSON response
	var stats KubeletStatsResponse
	if err := json.Unmarshal(rawBody, &stats); err != nil {
		return 0, fmt.Errorf("failed to parse kubelet stats: %w", err)
	}

	// Convert nanocores to millicores
	// 1 core = 1,000,000,000 nanocores = 1,000 millicores
	usageMillicores := int64(stats.Node.CPU.UsageNanoCores / 1000000)

	return usageMillicores, nil
}

// getNodeMemoryUsage queries kubelet stats API to get real-time memory usage
// Returns memory usage in bytes
func (r *NamespaceLifecyclePolicyReconciler) getNodeMemoryUsage(
	ctx context.Context,
	nodeName string,
) (int64, error) {
	if r.RESTClient == nil {
		return 0, fmt.Errorf("REST client not available")
	}

	result := r.RESTClient.Get().
		Resource("nodes").
		Name(nodeName).
		SubResource("proxy", "stats", "summary").
		Do(ctx)

	rawBody, err := result.Raw()
	if err != nil {
		return 0, fmt.Errorf("failed to query kubelet stats for node %s: %w", nodeName, err)
	}

	var stats KubeletStatsResponse
	if err := json.Unmarshal(rawBody, &stats); err != nil {
		return 0, fmt.Errorf("failed to parse kubelet stats: %w", err)
	}

	return int64(stats.Node.Memory.UsageBytes), nil
}

// hasSignal checks if a specific signal type exists in the signal list
func hasSignal(signals []appsv1alpha1.Signal, signalType appsv1alpha1.SignalType) bool {
	for _, signal := range signals {
		if signal.Type == signalType {
			return true
		}
	}
	return false
}
