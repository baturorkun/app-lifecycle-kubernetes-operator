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
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// collectSignals gathers all active signals from node conditions and pending pods
func (r *NamespaceLifecyclePolicyReconciler) collectSignals(
	ctx context.Context,
	config *appsv1alpha1.AdaptiveThrottlingConfig,
	targetNamespace string,
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

	// Sinyal 3: Pending Pods kontrolü
	if config.SignalChecks.CheckPendingPods != nil && config.SignalChecks.CheckPendingPods.Enabled {
		pendingSignal, err := r.checkPendingPods(ctx, targetNamespace, config.SignalChecks.CheckPendingPods.Threshold)
		if err != nil {
			return signals, fmt.Errorf("failed to check pending pods: %w", err)
		}
		if pendingSignal != nil {
			signals = append(signals, *pendingSignal)
		}
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

// checkPendingPods checks if there are too many pending pods in the namespace
func (r *NamespaceLifecyclePolicyReconciler) checkPendingPods(
	ctx context.Context,
	targetNamespace string,
	threshold int32,
) (*appsv1alpha1.Signal, error) {
	// List all pods in the target namespace
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
		return nil, err
	}

	// Count pending and unschedulable pods
	pendingCount := int32(0)
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodPending {
			// Check if pod is unschedulable
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
					condition.Reason == corev1.PodReasonUnschedulable {
					pendingCount++
					break
				}
			}
			// Also count pods that are simply pending (might be container creating, etc.)
			// This gives us an early warning
			if pendingCount == 0 {
				pendingCount++
			}
		}
	}

	// Check if we exceed threshold
	if pendingCount > threshold {
		return &appsv1alpha1.Signal{
			Type:     appsv1alpha1.SignalPendingPods,
			Severity: appsv1alpha1.SignalSeverityInfo,
			Count:    pendingCount,
			Message:  fmt.Sprintf("%d pods are pending in namespace %s (threshold: %d)", pendingCount, targetNamespace, threshold),
		}, nil
	}

	return nil, nil
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
