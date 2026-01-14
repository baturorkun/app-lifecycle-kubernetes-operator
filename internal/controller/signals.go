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
	"net/http"
	"strings"
	"time"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
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

// getNodeIP extracts the IP address from a Node object
// Prefers InternalIP, falls back to ExternalIP
func getNodeIP(node *corev1.Node) (string, error) {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address, nil
		}
	}
	// Fallback to ExternalIP if InternalIP not available
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeExternalIP {
			return addr.Address, nil
		}
	}
	return "", fmt.Errorf("no IP address found for node %s", node.Name)
}

// ThrottlingMetrics contains raw values from various checks for logging purposes
type ThrottlingMetrics struct {
	NotReadyNodes  int32
	PendingPods    int32
	MaxCPUPercent  int32
	MaxMemPercent  int32
	UnhealthyPods  int32
	RestartLatency int32 // Not used yet but good for future
}

// collectSignals gathers all active signals from node conditions and pending pods
func (r *NamespaceLifecyclePolicyReconciler) collectSignals(
	ctx context.Context,
	config *appsv1alpha1.AdaptiveThrottlingConfig,
	targetNamespace string,
) ([]appsv1alpha1.Signal, ThrottlingMetrics, error) {
	signals := []appsv1alpha1.Signal{}
	metrics := ThrottlingMetrics{}

	if config == nil || config.SignalChecks == nil {
		return signals, metrics, nil
	}

	// Sinyal 1: Node Ready kontrolü
	if config.SignalChecks.CheckNodeReady != nil && config.SignalChecks.CheckNodeReady.Enabled {
		nodeReadySignals, count, err := r.checkNodeReadiness(ctx, config.NodeSelector)
		if err != nil {
			return signals, metrics, fmt.Errorf("failed to check node readiness: %w", err)
		}
		signals = append(signals, nodeReadySignals...)
		metrics.NotReadyNodes = count
	}

	// Sinyal 2: Node Pressure kontrolü
	// Pressure has no single numeric metric other than presence, so we skip adding to metrics struct here
	if config.SignalChecks.CheckNodePressure != nil && config.SignalChecks.CheckNodePressure.Enabled {
		pressureSignals, err := r.checkNodePressure(ctx, config.NodeSelector, config.SignalChecks.CheckNodePressure.PressureTypes)
		if err != nil {
			return signals, metrics, fmt.Errorf("failed to check node pressure: %w", err)
		}
		signals = append(signals, pressureSignals...)
	}

	// Sinyal 3: Pending Pods kontrolü (cluster-wide)
	if config.SignalChecks.CheckPendingPods != nil && config.SignalChecks.CheckPendingPods.Enabled {
		pendingSignal, count, err := r.checkPendingPods(ctx, config.SignalChecks.CheckPendingPods.Threshold)
		if err != nil {
			return signals, metrics, fmt.Errorf("failed to check pending pods: %w", err)
		}
		if pendingSignal != nil {
			signals = append(signals, *pendingSignal)
		}
		metrics.PendingPods = count
	}

	// Sinyal 4: Node Usage kontrolü (PROACTIVE - real-time kubelet metrics)
	if config.SignalChecks.CheckNodeUsage != nil && config.SignalChecks.CheckNodeUsage.Enabled {
		var scrapeConfig *appsv1alpha1.MetricsScrapeConfig
		if config.SignalChecks.CheckNodeUsage.Scrape != nil {
			scrapeConfig = config.SignalChecks.CheckNodeUsage.Scrape
		}
		usageSignals, maxCPU, maxMem, err := r.checkNodeUsage(
			ctx,
			config.NodeSelector,
			config.SignalChecks.CheckNodeUsage.CPUThresholdPercent,
			config.SignalChecks.CheckNodeUsage.MemoryThresholdPercent,
			scrapeConfig,
		)
		if err != nil {
			return signals, metrics, fmt.Errorf("failed to check node usage: %w", err)
		}
		signals = append(signals, usageSignals...)
		metrics.MaxCPUPercent = maxCPU
		metrics.MaxMemPercent = maxMem
	}

	// Sinyal 5: Container Restarts kontrolü (only in targetNamespace)
	if config.SignalChecks.CheckContainerRestarts != nil && config.SignalChecks.CheckContainerRestarts.Enabled {
		restartSignals, unhealthyCount, err := r.checkContainerRestarts(ctx, targetNamespace, config.SignalChecks.CheckContainerRestarts.RestartThreshold)
		if err != nil {
			return signals, metrics, fmt.Errorf("failed to check container restarts: %w", err)
		}
		signals = append(signals, restartSignals...)
		metrics.UnhealthyPods = unhealthyCount
	}

	return signals, metrics, nil
}

// checkNodeReadiness checks if any nodes are NotReady
func (r *NamespaceLifecyclePolicyReconciler) checkNodeReadiness(
	ctx context.Context,
	nodeSelector map[string]string,
) ([]appsv1alpha1.Signal, int32, error) {
	signals := []appsv1alpha1.Signal{}
	notReadyCount := int32(0)

	// Default node selector to worker nodes if not specified
	if nodeSelector == nil {
		nodeSelector = map[string]string{"node-role.kubernetes.io/worker": ""}
	}

	// List nodes matching selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels(nodeSelector)); err != nil {
		return signals, notReadyCount, err
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
			notReadyCount++
			signals = append(signals, appsv1alpha1.Signal{
				Type:     appsv1alpha1.SignalNodeNotReady,
				Severity: appsv1alpha1.SignalSeverityCritical,
				Node:     node.Name,
				Message:  fmt.Sprintf("Node %s is NotReady", node.Name),
			})
		}
	}

	return signals, notReadyCount, nil
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
func (r *NamespaceLifecyclePolicyReconciler) checkPendingPods(
	ctx context.Context,
	threshold int32,
) (*appsv1alpha1.Signal, int32, error) {
	// List ALL pods in the cluster (cluster-wide)
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList); err != nil {
		return nil, 0, err
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

					// Make check case-insensitive to be more robust
					msg := strings.ToLower(condition.Message)
					if strings.Contains(msg, "insufficient cpu") ||
						strings.Contains(msg, "insufficient memory") ||
						strings.Contains(msg, "insufficient pods") {
						pendingCount++
						break
					}
				}
			}
		}
	}

	log := logf.FromContext(ctx)
	log.V(1).Info("Pending pods check result", "count", pendingCount, "threshold", threshold)

	// Check if we exceed threshold
	if pendingCount > threshold {
		return &appsv1alpha1.Signal{
			Type:     appsv1alpha1.SignalPendingPods,
			Severity: appsv1alpha1.SignalSeverityInfo,
			Count:    pendingCount,
			Message:  fmt.Sprintf("%d pods are pending cluster-wide due to resource constraints (threshold: %d)", pendingCount, threshold),
		}, pendingCount, nil
	}

	return nil, pendingCount, nil
}

// checkContainerRestarts checks if any pods are in CrashLoopBackOff or have high restart counts
// Only checks pods in the targetNamespace
func (r *NamespaceLifecyclePolicyReconciler) checkContainerRestarts(
	ctx context.Context,
	targetNamespace string,
	threshold int32,
) ([]appsv1alpha1.Signal, int32, error) {
	signals := []appsv1alpha1.Signal{}
	unhealthyCount := int32(0)

	// List pods only in the targetNamespace
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList, client.InNamespace(targetNamespace)); err != nil {
		return signals, unhealthyCount, err
	}

	if threshold <= 0 {
		threshold = 10 // safety default
	}

	for _, pod := range podList.Items {
		for _, status := range pod.Status.ContainerStatuses {
			isCrashLoop := false
			if status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
				isCrashLoop = true
			}

			if isCrashLoop || status.RestartCount >= threshold {
				unhealthyCount++
				msg := fmt.Sprintf("Pod %s/%s container %s is unhealthy (Restarts: %d, CrashLoop: %v)",
					pod.Namespace, pod.Name, status.Name, status.RestartCount, isCrashLoop)

				signals = append(signals, appsv1alpha1.Signal{
					Type:     appsv1alpha1.SignalContainerRestarts,
					Severity: appsv1alpha1.SignalSeverityWarning,
					Node:     pod.Spec.NodeName,
					Count:    status.RestartCount,
					Message:  msg,
				})
				// One unhealthy container per pod is enough to trigger the signal for that pod
				break
			}
		}
	}

	if len(signals) > 0 {
		log := logf.FromContext(ctx)
		log.V(1).Info("Container restarts check result", "namespace", targetNamespace, "count", len(signals), "threshold", threshold)
	}

	return signals, unhealthyCount, nil
}

// checkNodeUsage checks real-time CPU/Memory usage from kubelet or alternative source
func (r *NamespaceLifecyclePolicyReconciler) checkNodeUsage(
	ctx context.Context,
	nodeSelector map[string]string,
	cpuThreshold int32,
	memoryThreshold int32,
	scrapeConfig *appsv1alpha1.MetricsScrapeConfig,
) ([]appsv1alpha1.Signal, int32, int32, error) {
	signals := []appsv1alpha1.Signal{}
	maxCPU := int32(0)
	maxMem := int32(0)

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

	// List nodes matching selector
	nodeList := &corev1.NodeList{}
	if err := r.List(ctx, nodeList, client.MatchingLabels(nodeSelector)); err != nil {
		return signals, maxCPU, maxMem, err
	}

	// Fallsback: If no worker nodes found (common in local single-node clusters), list ALL nodes
	if len(nodeList.Items) == 0 {
		log := logf.FromContext(ctx)
		log.V(1).Info("No nodes found with worker label, falling back to all nodes")
		if err := r.List(ctx, nodeList); err != nil {
			return signals, maxCPU, maxMem, err
		}
	}

	// Check EACH NODE separately
	for _, node := range nodeList.Items {
		log := logf.FromContext(ctx) // This logger might be framework-polluted, but it's for internal debug
		// Get node allocatable resources
		allocatableCPU := node.Status.Allocatable.Cpu()
		allocatableMemory := node.Status.Allocatable.Memory()

		if allocatableCPU.IsZero() || allocatableMemory.IsZero() {
			continue // Skip if resources not available
		}

		// Determine if we're using URL source (percentage values) or kubelet (raw values)
		isURLSource := false
		if scrapeConfig != nil && scrapeConfig.Source != "" && scrapeConfig.Source != "kubelet" {
			isURLSource = true
		}

		var cpuPercent, memoryPercent int32

		if isURLSource {
			// URL source: Get node IP and fetch metrics from node's IP
			nodeIP, err := getNodeIP(&node)
			if err != nil {
				log.Info("⚠️ Skipping node - failed to get IP", "node", node.Name, "error", err)
				continue // Skip this node, don't include in check
			}

			// Fetch CPU usage from node's IP
			cpuPercentFloat, err := r.getNodeCPUUsageFromURL(ctx, &node, scrapeConfig.Source, scrapeConfig)
			if err != nil {
				log.Info("⚠️ Skipping node - failed to get CPU usage from URL", "node", node.Name, "nodeIP", nodeIP, "source", scrapeConfig.Source, "error", err)
				continue // Skip this node, don't include in check
			}
			cpuPercent = int32(cpuPercentFloat)

			// Fetch Memory usage from node's IP
			memPercentFloat, err := r.getNodeMemoryUsageFromURL(ctx, &node, scrapeConfig.Source, scrapeConfig)
			if err != nil {
				log.Info("⚠️ Skipping node - failed to get Memory usage from URL", "node", node.Name, "nodeIP", nodeIP, "source", scrapeConfig.Source, "error", err)
				continue // Skip this node, don't include in check
			}
			memoryPercent = int32(memPercentFloat)

			// Log successful fetch with both CPU and Memory together
			log.Info("✅ Successfully scraped metrics from URL for node",
				"node", node.Name,
				"nodeIP", nodeIP,
				"cpuPercent", cpuPercent,
				"memPercent", memoryPercent,
				"source", scrapeConfig.Source)
		} else {
			// Kubelet source returns raw values (millicores/bytes)
			usedCPU, err := r.getNodeCPUUsage(ctx, node.Name, scrapeConfig)
			if err != nil {
				log.Info("⚠️ Failed to get CPU usage for node", "node", node.Name, "error", err)
				continue
			}
			if usedCPU == 0 {
				log.Info("⚠️ CPU usage is 0 for node (possible RKE2 issue)", "node", node.Name)
			}

			usedMemory, err := r.getNodeMemoryUsage(ctx, node.Name, scrapeConfig)
			if err != nil {
				log.Info("⚠️ Failed to get Memory usage for node", "node", node.Name, "error", err)
				continue
			}
			if usedMemory == 0 {
				log.Info("⚠️ Memory usage is 0 for node (possible RKE2 issue)", "node", node.Name)
			}

			// Calculate REAL usage percentages from raw values
			cpuPercent = int32((usedCPU * 100) / allocatableCPU.MilliValue())
			memoryPercent = int32((usedMemory * 100) / allocatableMemory.Value())
		}

		// Track max values
		if cpuPercent > maxCPU {
			maxCPU = cpuPercent
		}
		if memoryPercent > maxMem {
			maxMem = memoryPercent
		}

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

	return signals, maxCPU, maxMem, nil
}

// getNodeCPUUsage queries kubelet stats API to get real-time CPU usage
// Returns CPU usage in millicores (e.g., 1500 = 1.5 CPU cores)
// Note: URL sources are handled directly in checkNodeUsage
func (r *NamespaceLifecyclePolicyReconciler) getNodeCPUUsage(
	ctx context.Context,
	nodeName string,
	scrapeConfig *appsv1alpha1.MetricsScrapeConfig,
) (int64, error) {
	log := logf.FromContext(ctx)

	// Use Kubernetes API server proxy to kubelet (default behavior)
	log.V(1).Info("Fetching CPU usage from kubelet", "node", nodeName, "source", "kubelet")
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
		// Log first 500 chars of response for debugging RKE2 issues
		responsePreview := string(rawBody)
		if len(responsePreview) > 500 {
			responsePreview = responsePreview[:500] + "..."
		}
		log.Info("⚠️ Failed to parse kubelet stats response", "node", nodeName, "responsePreview", responsePreview, "error", err)
		return 0, fmt.Errorf("failed to parse kubelet stats: %w", err)
	}

	// Convert nanocores to millicores
	// 1 core = 1,000,000,000 nanocores = 1,000 millicores
	usageMillicores := int64(stats.Node.CPU.UsageNanoCores / 1000000)

	// Debug: Log if usage is 0 (common in RKE2)
	if usageMillicores == 0 {
		log.V(1).Info("CPU usage is 0 from kubelet stats", "node", nodeName, "usageNanoCores", stats.Node.CPU.UsageNanoCores)
	}

	return usageMillicores, nil
}

// getNodeCPUUsageFromURL fetches CPU usage percentage from a custom URL
// Returns CPU usage as percentage (float64)
// portAndPath should be in format ":9090/metrics" (port and path, no IP)
func (r *NamespaceLifecyclePolicyReconciler) getNodeCPUUsageFromURL(
	ctx context.Context,
	node *corev1.Node,
	portAndPath string,
	scrapeConfig *appsv1alpha1.MetricsScrapeConfig,
) (float64, error) {
	if scrapeConfig == nil || scrapeConfig.CPU == "" {
		return 0, fmt.Errorf("scrape.cpu path is required when using URL source")
	}

	// Get node IP
	nodeIP, err := getNodeIP(node)
	if err != nil {
		return 0, fmt.Errorf("failed to get IP for node %s: %w", node.Name, err)
	}

	// Construct full URL: http://{node-ip}{port+path}
	fullURL := fmt.Sprintf("http://%s%s", nodeIP, portAndPath)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Make HTTP GET request
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request to %s: %w", fullURL, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch from %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, fullURL)
	}

	// Parse JSON response
	var jsonData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&jsonData); err != nil {
		return 0, fmt.Errorf("failed to parse JSON response from %s: %w", fullURL, err)
	}

	// Extract CPU percentage using JSON path
	cpuPercent, err := extractJSONPath(jsonData, scrapeConfig.CPU)
	if err != nil {
		return 0, fmt.Errorf("failed to extract CPU path '%s' from response: %w", scrapeConfig.CPU, err)
	}

	return cpuPercent, nil
}

// getNodeMemoryUsage queries kubelet stats API to get real-time memory usage
// Returns memory usage in bytes
// Note: URL sources are handled directly in checkNodeUsage
func (r *NamespaceLifecyclePolicyReconciler) getNodeMemoryUsage(
	ctx context.Context,
	nodeName string,
	scrapeConfig *appsv1alpha1.MetricsScrapeConfig,
) (int64, error) {
	log := logf.FromContext(ctx)

	// Use Kubernetes API server proxy to kubelet (default behavior)
	log.V(1).Info("Fetching memory usage from kubelet", "node", nodeName, "source", "kubelet")
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
		// Log first 500 chars of response for debugging RKE2 issues
		responsePreview := string(rawBody)
		if len(responsePreview) > 500 {
			responsePreview = responsePreview[:500] + "..."
		}
		log.Info("⚠️ Failed to parse kubelet stats response", "node", nodeName, "responsePreview", responsePreview, "error", err)
		return 0, fmt.Errorf("failed to parse kubelet stats: %w", err)
	}

	memoryBytes := int64(stats.Node.Memory.UsageBytes)

	// Debug: Log if usage is 0 (common in RKE2)
	if memoryBytes == 0 {
		log.V(1).Info("Memory usage is 0 from kubelet stats", "node", nodeName, "usageBytes", stats.Node.Memory.UsageBytes)
	}

	return memoryBytes, nil
}

// getNodeMemoryUsageFromURL fetches memory usage percentage from a custom URL
// Returns memory usage as percentage (float64)
// portAndPath should be in format ":9090/metrics" (port and path, no IP)
func (r *NamespaceLifecyclePolicyReconciler) getNodeMemoryUsageFromURL(
	ctx context.Context,
	node *corev1.Node,
	portAndPath string,
	scrapeConfig *appsv1alpha1.MetricsScrapeConfig,
) (float64, error) {
	if scrapeConfig == nil || scrapeConfig.Mem == "" {
		return 0, fmt.Errorf("scrape.mem path is required when using URL source")
	}

	// Get node IP
	nodeIP, err := getNodeIP(node)
	if err != nil {
		return 0, fmt.Errorf("failed to get IP for node %s: %w", node.Name, err)
	}

	// Construct full URL: http://{node-ip}{port+path}
	fullURL := fmt.Sprintf("http://%s%s", nodeIP, portAndPath)

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Make HTTP GET request
	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request to %s: %w", fullURL, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch from %s: %w", fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, fullURL)
	}

	// Parse JSON response
	var jsonData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&jsonData); err != nil {
		return 0, fmt.Errorf("failed to parse JSON response from %s: %w", fullURL, err)
	}

	// Extract memory percentage using JSON path
	memPercent, err := extractJSONPath(jsonData, scrapeConfig.Mem)
	if err != nil {
		return 0, fmt.Errorf("failed to extract memory path '%s' from response: %w", scrapeConfig.Mem, err)
	}

	return memPercent, nil
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

// extractJSONPath extracts a value from JSON using a dot-separated path (e.g., "a.b.c")
// Returns the value as float64, or error if path not found
func extractJSONPath(data map[string]interface{}, path string) (float64, error) {
	keys := strings.Split(path, ".")
	current := interface{}(data)

	for i, key := range keys {
		obj, ok := current.(map[string]interface{})
		if !ok {
			return 0, fmt.Errorf("path segment '%s' at position %d is not an object", key, i)
		}

		value, exists := obj[key]
		if !exists {
			return 0, fmt.Errorf("key '%s' not found at path segment %d", key, i)
		}

		// If this is the last key, return the value
		if i == len(keys)-1 {
			switch v := value.(type) {
			case float64:
				return v, nil
			case int:
				return float64(v), nil
			case int32:
				return float64(v), nil
			case int64:
				return float64(v), nil
			default:
				return 0, fmt.Errorf("value at path '%s' is not a number (got %T)", path, value)
			}
		}

		current = value
	}

	return 0, fmt.Errorf("unexpected end of path '%s'", path)
}
