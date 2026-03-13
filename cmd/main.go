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

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"os"
	"sort"
	"sync"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	"go.uber.org/zap/zapcore"
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
	"github.com/baturorkun/app-lifecycle-kubernetes-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(appsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// waitForReplicasZero waits for all Deployments and StatefulSets in a namespace to have replicas=0
// This is safer than waiting for pod termination as it doesn't get stuck on terminating pods
// Returns true if all workloads are scaled to 0, false if timeout or error
func waitForReplicasZero(ctx context.Context, k8sClient client.Client, namespace string, selector *metav1.LabelSelector, logger logr.Logger) bool {
	maxWaitTime := 5 * time.Minute
	startTime := time.Now()
	pollInterval := 2 * time.Second

	logger.Info("⏳ Waiting for all workloads to scale to 0 replicas", "namespace", namespace)

	for {
		// Check timeout
		if time.Since(startTime) >= maxWaitTime {
			logger.Info("⚠️ Timeout waiting for replicas to reach 0", "namespace", namespace, "maxWaitTime", maxWaitTime)
			return false
		}

		// Prepare list options with label selector
		listOpts := []client.ListOption{
			client.InNamespace(namespace),
		}

		if selector != nil {
			labelSelector, err := metav1.LabelSelectorAsSelector(selector)
			if err != nil {
				logger.Error(err, "Failed to convert label selector", "namespace", namespace)
				return false
			}
			listOpts = append(listOpts, client.MatchingLabelsSelector{Selector: labelSelector})
		}

		// List Deployments
		deploymentList := &appsv1.DeploymentList{}
		if err := k8sClient.List(ctx, deploymentList, listOpts...); err != nil {
			logger.Error(err, "Failed to list deployments", "namespace", namespace)
			return false
		}

		// List StatefulSets
		statefulSetList := &appsv1.StatefulSetList{}
		if err := k8sClient.List(ctx, statefulSetList, listOpts...); err != nil {
			logger.Error(err, "Failed to list statefulsets", "namespace", namespace)
			return false
		}

		// Check if all Deployments are scaled to 0
		allDeploymentsZero := true
		for i := range deploymentList.Items {
			deploy := &deploymentList.Items[i]
			// Only check operational replicas (available/ready), not total replicas
			// This prevents hanging on pods stuck in Terminating state
			if deploy.Status.AvailableReplicas > 0 || deploy.Status.ReadyReplicas > 0 {
				allDeploymentsZero = false
				logger.V(1).Info("Deployment has active replicas",
					"deployment", deploy.Name,
					"available", deploy.Status.AvailableReplicas,
					"ready", deploy.Status.ReadyReplicas)
			}
		}

		// Check if all StatefulSets are scaled to 0
		allStatefulSetsZero := true
		for i := range statefulSetList.Items {
			sts := &statefulSetList.Items[i]
			// Only check ready replicas, not total replicas
			// This prevents hanging on pods stuck in Terminating state
			if sts.Status.ReadyReplicas > 0 {
				allStatefulSetsZero = false
				logger.V(1).Info("StatefulSet has active replicas",
					"statefulset", sts.Name,
					"ready", sts.Status.ReadyReplicas)
			}
		}

		// If all workloads are at 0 replicas, we're done
		if allDeploymentsZero && allStatefulSetsZero {
			logger.Info("✅ All workloads scaled to 0 replicas",
				"namespace", namespace,
				"waited", time.Since(startTime),
				"deployments", len(deploymentList.Items),
				"statefulsets", len(statefulSetList.Items))
			return true
		}

		logger.V(1).Info("Waiting for workloads to scale to 0",
			"namespace", namespace,
			"deployments", len(deploymentList.Items),
			"statefulsets", len(statefulSetList.Items))
		time.Sleep(pollInterval)
	}
}

// processPolicyWithDelayAndFreeze processes a single freeze policy: applies startup policy, waits for delay, and polls for freeze completion
func processPolicyWithDelayAndFreeze(ctx context.Context, logger logr.Logger, reconciler *controller.NamespaceLifecyclePolicyReconciler, k8sClient client.Client, policy *appsv1alpha1.NamespaceLifecyclePolicy, priority int32) {
	logger.Info("Processing freeze policy", "policy", policy.Name, "priority", priority)

	if err := reconciler.ApplyStartupPolicy(ctx, policy); err != nil {
		logger.Error(err, "Failed to apply startup freeze policy", "policy", policy.Name)
		return
	}

	// Re-fetch the policy to get updated status
	latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
		logger.Error(err, "Failed to re-fetch policy after ApplyStartupPolicy", "policy", policy.Name)
		return
	}

	// Wait for this policy to complete (delay + freeze) - only if startupPolicy is Freeze
	if latestPolicy.Spec.StartupPolicy == appsv1alpha1.StartupPolicyFreeze {
		logger.Info("⏳ Waiting for freeze policy to complete", "policy", latestPolicy.Name, "priority", priority, "delay", latestPolicy.Spec.FreezeDelay.Duration)

		// Wait for delay to complete (if any)
		if latestPolicy.Spec.FreezeDelay.Duration > 0 {
			delayDuration := latestPolicy.Spec.FreezeDelay.Duration
			logger.Info("⏱️ Waiting for startup freeze delay", "policy", latestPolicy.Name, "delay", delayDuration)
			time.Sleep(delayDuration)
			logger.Info("✅ Freeze delay completed", "policy", latestPolicy.Name)

			// Re-fetch after delay
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				logger.Error(err, "Failed to re-fetch policy after delay", "policy", policy.Name)
				return
			}
		}

		// Wait for freeze to complete (Phase = Frozen)
		logger.Info("⏳ Waiting for freeze to complete", "policy", latestPolicy.Name)
		maxWaitTime := 10 * time.Minute
		startTime := time.Now()
		pollInterval := 2 * time.Second

		for {
			// Check timeout
			if time.Since(startTime) >= maxWaitTime {
				logger.Info("⚠️ Timeout waiting for freeze to complete", "policy", latestPolicy.Name, "maxWaitTime", maxWaitTime)
				break
			}

			// Re-fetch to check status
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				logger.Error(err, "Failed to re-fetch policy during freeze wait", "policy", policy.Name)
				break
			}

			// Check if freeze is complete
			if latestPolicy.Status.Phase == appsv1alpha1.PhaseFrozen {
				logger.Info("✅ Freeze completed", "policy", latestPolicy.Name, "waited", time.Since(startTime))
				break
			}

			// Check if failed
			if latestPolicy.Status.Phase == appsv1alpha1.PhaseFailed {
				logger.Info("⚠️ Freeze failed", "policy", latestPolicy.Name)
				break
			}

			time.Sleep(pollInterval)
		}

		// Wait for all workloads (Deployments/StatefulSets) to be scaled to 0 replicas
		// This ensures the next priority group doesn't start until workloads are fully scaled down
		// Using replicas=0 check instead of pod termination to avoid hanging on stuck pods
		logger.Info("🔍 Verifying workloads scaled to 0", "namespace", latestPolicy.Spec.TargetNamespace)
		if !waitForReplicasZero(ctx, k8sClient, latestPolicy.Spec.TargetNamespace, latestPolicy.Spec.Selector, logger) {
			logger.Info("⚠️ Replicas verification timed out, continuing anyway", "namespace", latestPolicy.Spec.TargetNamespace)
		}
	}
}

// processPolicyWithDelayAndResume processes a single policy: applies startup policy, waits for delay, and polls for resume completion
// applyStartupPolicies applies startup policies for all existing NamespaceLifecyclePolicy resources
// This runs once when the operator starts, before the controller starts processing events
// Freeze policies are processed first (by freezePriority: 0, 1, 2...), then resume policies (by resumePriority)
func applyStartupPolicies(ctx context.Context, mgr manager.Manager) error {
	setupLog := ctrl.Log.WithName("startup")
	startTime := time.Now()
	setupLog.Info("Applying startup policies for existing resources")

	// Create a client
	k8sClient := mgr.GetClient()
	apiReader := mgr.GetAPIReader() // bypasses cache — used for node listing to avoid stale state

	// List all NamespaceLifecyclePolicy resources
	policyList := &appsv1alpha1.NamespaceLifecyclePolicyList{}
	if err := k8sClient.List(ctx, policyList); err != nil {
		setupLog.Error(err, "Failed to list NamespaceLifecyclePolicy resources")
		return err
	}

	setupLog.Info("Found policies", "count", len(policyList.Items))

	// Create reconciler instance to use helper functions
	reconciler := &controller.NamespaceLifecyclePolicyReconciler{
		Client:    k8sClient,
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}

	// Separate freeze and resume policies
	var freezePolicies []*appsv1alpha1.NamespaceLifecyclePolicy
	var resumePolicies []*appsv1alpha1.NamespaceLifecyclePolicy
	var ignorePolicies []*appsv1alpha1.NamespaceLifecyclePolicy

	for i := range policyList.Items {
		policy := &policyList.Items[i]
		switch policy.Spec.StartupPolicy {
		case appsv1alpha1.StartupPolicyFreeze:
			freezePolicies = append(freezePolicies, policy)
		case appsv1alpha1.StartupPolicyResume:
			resumePolicies = append(resumePolicies, policy)
		case appsv1alpha1.StartupPolicyIgnore:
			ignorePolicies = append(ignorePolicies, policy)
		}
	}

	setupLog.Info("Policies categorized by startup policy",
		"freeze", len(freezePolicies),
		"resume", len(resumePolicies),
		"ignore", len(ignorePolicies))

	// ============================================================================
	// STARTUP FREEZE: Process FREEZE policies by priority (0, 1, 2, ...)
	// ============================================================================
	if len(freezePolicies) > 0 {
		setupLog.Info("🥶 ========== STARTUP FREEZE: PROCESSING FREEZE POLICIES ==========")

		// Sort freeze policies by freezePriority (lower number = freeze first: 0, then 1, then 2...)
		sort.Slice(freezePolicies, func(i, j int) bool {
			priorityI := freezePolicies[i].Spec.FreezePriority
			priorityJ := freezePolicies[j].Spec.FreezePriority

			// Sort by priority first (ascending: 0, 1, 2...)
			if priorityI != priorityJ {
				return priorityI < priorityJ
			}

			// Same priority: sort by creation timestamp (older first)
			return freezePolicies[i].CreationTimestamp.Before(&freezePolicies[j].CreationTimestamp)
		})

		// Group freeze policies by priority
		freezePriorityGroups := make(map[int32][]*appsv1alpha1.NamespaceLifecyclePolicy)
		freezePriorityOrder := []int32{}

		for _, policy := range freezePolicies {
			priority := policy.Spec.FreezePriority

			if _, exists := freezePriorityGroups[priority]; !exists {
				freezePriorityGroups[priority] = []*appsv1alpha1.NamespaceLifecyclePolicy{}
				freezePriorityOrder = append(freezePriorityOrder, priority)
			}
			freezePriorityGroups[priority] = append(freezePriorityGroups[priority], policy)
		}

		// Sort priority order (ascending: 0, 1, 2...)
		sort.Slice(freezePriorityOrder, func(i, j int) bool {
			return freezePriorityOrder[i] < freezePriorityOrder[j]
		})

		setupLog.Info("Freeze policies grouped by priority", "groups", len(freezePriorityOrder))

		// Process each freeze priority group sequentially
		for _, priority := range freezePriorityOrder {
			group := freezePriorityGroups[priority]
			setupLog.Info("🥶 Processing freeze priority group", "priority", priority, "policies", len(group))

			// Process all policies in this priority group in parallel
			var wg sync.WaitGroup
			for _, policy := range group {
				wg.Add(1)
				go func(p *appsv1alpha1.NamespaceLifecyclePolicy) {
					defer wg.Done()
					policyLogger := setupLog.WithValues("policy", p.Name, "freezePriority", priority)
					processPolicyWithDelayAndFreeze(ctx, policyLogger, reconciler, k8sClient, p, priority)
				}(policy)
			}

			// Wait for all policies in this freeze priority group to complete
			wg.Wait()
			setupLog.Info("✅ Freeze priority group completed", "priority", priority, "policies", len(group))
		}

		setupLog.Info("✅ ========== FREEZE POLICIES COMPLETED ==========")
	}

	// ============================================================================
	// NODE FAILURE PRE-SCAN
	// Detect NotReady nodes BEFORE processing resume policies.
	// Policies with handleNodeFailure=true are scaled down SYNCHRONOUSLY here, before
	// STARTUP RESUME processes any workloads.  The scale-down removes fully-local workloads from
	// the failed node so that STARTUP RESUME only resumes workloads destined for healthy nodes.
	// After scale-down the reconcile loop re-resumes via PendingStartupResume=true.
	// ============================================================================
	// Use APIReader (direct API server) instead of cached client to get the actual current
	// node state. When the operator pod is rescheduled after a node failure, the cache
	// may still show the failed node as Ready before it propagates the NotReady status.
	nodeList := &corev1.NodeList{}
	if err := apiReader.List(ctx, nodeList); err != nil {
		setupLog.Error(err, "Startup: failed to list nodes for node failure pre-scan — skipping")
	} else {
		var failedNodeNames []string
		for i := range nodeList.Items {
			node := &nodeList.Items[i]
			// Check NodeReady condition
			for _, cond := range node.Status.Conditions {
				if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
					failedNodeNames = append(failedNodeNames, node.Name)
					break
				}
			}
		}

		if len(failedNodeNames) > 0 {
			setupLog.Info("🔴 Startup: NotReady nodes detected — scaling down affected workloads BEFORE startup resume",
				"failedNodes", failedNodeNames)

			failedNode := failedNodeNames[0] // use the first failed node; real-time watcher handles subsequent ones

			var filteredResumePolicies []*appsv1alpha1.NamespaceLifecyclePolicy
			for _, policy := range resumePolicies {
				if !policy.Spec.HandleNodeFailure {
					filteredResumePolicies = append(filteredResumePolicies, policy)
					continue
				}

				setupLog.Info("🔴 Startup pre-scan: performing synchronous scale-down before resume",
					"policy", policy.Name,
					"failedNode", failedNode)

				// Idempotency: only run if not already handled for this exact failure
				alreadyHandled := policy.Status.FailedNodeName == failedNode &&
					policy.Status.NodeFailureEventHandledAt != nil &&
					(policy.Status.NodeFailureEventDetectedAt == nil ||
						policy.Status.NodeFailureEventHandledAt.After(policy.Status.NodeFailureEventDetectedAt.Time))

				if !alreadyHandled {
					if err := reconciler.HandleNodeFailureAtStartup(ctx, policy, failedNode); err != nil {
						setupLog.Error(err, "Startup pre-scan: failed to handle node failure for policy", "policy", policy.Name)
						// Still suppress resume on failure — safer than resuming into a degraded cluster
					}
				} else {
					setupLog.Info("✅ Startup pre-scan: node failure already handled for this failure event — skipping scale-down",
						"policy", policy.Name,
						"failedNode", failedNode)
				}
				// Do NOT add to filteredResumePolicies — startup resume is suppressed here;
				// the reconcile loop will re-resume via PendingStartupResume=true.
			}
			resumePolicies = filteredResumePolicies
		} else {
			setupLog.Info("✅ Startup: all nodes are Ready — node failure pre-scan passed")
		}
	}

	// ============================================================================
	// STARTUP RESUME: Set PendingStartupResume=true on all resume policies.
	// Priority ordering is enforced by the reconciler's isBlockedByHigherPriorityResume
	// gate, which is the same code path used by node failure recovery.
	// ============================================================================
	if len(resumePolicies) > 0 {
		setupLog.Info("🚀 ========== STARTUP RESUME: MARKING POLICIES AS PENDING ==========")

		// Sort ascending so the highest-priority policy (lowest number) is flagged first.
		// This ensures its reconcile event fires and starts resuming before lower-priority
		// policies are flagged, so the gate reliably sees it as pending when they check.
		sort.Slice(resumePolicies, func(i, j int) bool {
			pi, pj := resumePolicies[i].Spec.ResumePriority, resumePolicies[j].Spec.ResumePriority
			if pi == 0 {
				pi = 100
			}
			if pj == 0 {
				pj = 100
			}
			return pi < pj
		})

		for _, policy := range resumePolicies {
			policyLogger := setupLog.WithValues("policy", policy.Name, "resumePriority", policy.Spec.ResumePriority)
			if err := reconciler.ApplyStartupPolicy(ctx, policy); err != nil {
				policyLogger.Error(err, "Failed to mark policy as pending startup resume")
			}
		}

		setupLog.Info("✅ ========== STARTUP RESUME: ALL POLICIES MARKED PENDING — RECONCILER HANDLES ORDERING ==========")
	}

	setupLog.Info("Startup policy check completed",
		"totalElapsedTime", time.Since(startTime))

	// Trigger reconciles for policies with pending pre-conditions
	// This ensures the reconcile loop picks up and continues checking
	pendingList := &appsv1alpha1.NamespaceLifecyclePolicyList{}
	if err := k8sClient.List(ctx, pendingList); err != nil {
		setupLog.Error(err, "Failed to list policies for pending pre-conditions check")
	} else {
		for i := range pendingList.Items {
			p := &pendingList.Items[i]
			if p.Status.PreConditionsStatus != nil && p.Status.PreConditionsStatus.Checking {
				setupLog.Info("🔄 Triggering reconcile for policy with pending pre-conditions",
					"policy", p.Name)
				// Re-fetch to get the latest resource version before updating the annotation,
				// since concurrent status updates (e.g. HandleNodeFailureAtStartup) may have
				// modified the object and our list copy is stale.
				latest := &appsv1alpha1.NamespaceLifecyclePolicy{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: p.Name, Namespace: p.Namespace}, latest); err != nil {
					setupLog.Error(err, "Failed to re-fetch policy for reconcile trigger", "policy", p.Name)
					continue
				}
				if latest.Annotations == nil {
					latest.Annotations = make(map[string]string)
				}
				latest.Annotations["apps.ops.dev/precondition-trigger"] = time.Now().Format(time.RFC3339)
				if err := k8sClient.Update(ctx, latest); err != nil {
					setupLog.Error(err, "Failed to trigger reconcile for policy", "policy", p.Name)
				}
			}
		}
	}

	return nil
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var debug bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&debug, "debug", false, "Enable debug logging.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Set level: Default to Info, switch to Debug if flag or env is set
	logLevel := zapcore.InfoLevel
	if debug || os.Getenv("DEBUG") == "true" {
		logLevel = zapcore.DebugLevel
	}
	opts.Level = logLevel

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c9271d08.ops.dev",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Create Kubernetes clientset for REST client access
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		setupLog.Error(err, "unable to create kubernetes clientset")
		os.Exit(1)
	}

	mainReconciler := &controller.NamespaceLifecyclePolicyReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		RESTClient: clientset.CoreV1().RESTClient(),
		APIReader:  mgr.GetAPIReader(),
	}
	if err := mainReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "NamespaceLifecyclePolicy")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Add startup policy runnable - will run after cache is started
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		// Wait for cache to sync
		if !mgr.GetCache().WaitForCacheSync(ctx) {
			setupLog.Error(nil, "Failed to wait for cache sync")
			return nil // Don't fail the manager
		}

		// On clusters with leader election (production RKE2 etc.), the standby operator
		// can win the leader lease in ~15s — before kube-controller-manager marks the
		// failed node as NotReady (default node-monitor-grace-period: 40s).
		// Poll the node list every 10s for up to 50s. Exit early if a NotReady node is
		// found; otherwise wait the full 50s to be sure no failure is pending.
		// On production clusters with leader election (RKE2 etc.), the standby operator
		// can win the leader lease in ~15s — before kube-controller-manager marks the
		// failed node as NotReady (default node-monitor-grace-period: 40s).
		// Poll every 10s for up to 50s. Exit early on any sign of node failure:
		// either a non-True NodeReady condition OR an unreachable/not-ready taint.
		{
			const maxWait = 50 * time.Second
			const pollInterval = 10 * time.Second
			deadline := time.Now().Add(maxWait)
			setupLog.Info("startup node-scan: polling nodes (max 50s, early exit on failure)", "pollInterval", "10s")
			for {
				nodeList := &corev1.NodeList{}
				if err := mgr.GetAPIReader().List(ctx, nodeList); err != nil {
					setupLog.Error(err, "startup node-scan: failed to list nodes, retrying")
				} else {
					for _, node := range nodeList.Items {
						// Check 1: NodeReady condition
						readyStatus := corev1.ConditionUnknown
						for _, cond := range node.Status.Conditions {
							if cond.Type == corev1.NodeReady {
								readyStatus = cond.Status
								break
							}
						}
						// Check 2: unreachable / not-ready taint (applied by kube-controller-manager
						// at the same time or slightly before the condition update)
						hasFaultTaint := false
						for _, taint := range node.Spec.Taints {
							if taint.Key == "node.kubernetes.io/unreachable" || taint.Key == "node.kubernetes.io/not-ready" {
								hasFaultTaint = true
								break
							}
						}
						setupLog.Info("startup node-scan", "node", node.Name, "readyStatus", string(readyStatus), "faultTaint", hasFaultTaint)
						if readyStatus != corev1.ConditionTrue || hasFaultTaint {
							setupLog.Info("startup node-scan: unhealthy node detected — running failure scan now", "node", node.Name, "readyStatus", string(readyStatus), "faultTaint", hasFaultTaint)
							goto runStartup
						}
					}
				}
				if time.Now().After(deadline) {
					setupLog.Info("startup node-scan: all nodes healthy after 50s — proceeding normally")
					break
				}
				select {
				case <-time.After(pollInterval):
				case <-ctx.Done():
					return nil
				}
			}
		}
	runStartup:

		// Apply startup policies
		if err := applyStartupPolicies(ctx, mgr); err != nil {
			setupLog.Error(err, "Failed to apply startup policies")
		}
		// Signal that startup is complete — reconciler may now treat unhandled
		// operationIds as live manual commands and cancel startup resume if needed.
		mainReconciler.MarkStartupComplete()
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to add startup policy runnable")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
