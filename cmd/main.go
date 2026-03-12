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

// NodeFailurePreScan inspects the supplied resume policies for NotReady nodes and
// handles any with handleNodeFailure=true synchronously.  It returns a filtered slice
// containing only those policies that should still be processed by the normal startup
// resume loop (i.e. ones with handleNodeFailure==false).  The returned string slice is
// the list of policy names handled in the order they were processed; tests rely on the
// ordering to verify priority.
func NodeFailurePreScan(ctx context.Context, k8sClient client.Client, resumePolicies []*appsv1alpha1.NamespaceLifecyclePolicy) ([]*appsv1alpha1.NamespaceLifecyclePolicy, []string, error) {
	// sort by startupResumePriority (lower number = higher priority)
	sort.Slice(resumePolicies, func(i, j int) bool {
		pi := resumePolicies[i].Spec.StartupResumePriority
		if pi == 0 {
			pi = 100
		}
		pj := resumePolicies[j].Spec.StartupResumePriority
		if pj == 0 {
			pj = 100
		}
		if pi != pj {
			return pi < pj
		}
		return resumePolicies[i].CreationTimestamp.Before(&resumePolicies[j].CreationTimestamp)
	})

	var handledOrder []string
	filtered := make([]*appsv1alpha1.NamespaceLifecyclePolicy, 0, len(resumePolicies))

	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList); err != nil {
		// if we can't list nodes just return original slice unchanged
		return resumePolicies, handledOrder, err
	}
	var failedNodeNames []string
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		for _, cond := range node.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status != corev1.ConditionTrue {
				failedNodeNames = append(failedNodeNames, node.Name)
				break
			}
		}
	}
	if len(failedNodeNames) == 0 {
		return resumePolicies, handledOrder, nil
	}

	failedNode := failedNodeNames[0]
	for _, policy := range resumePolicies {
		if !policy.Spec.HandleNodeFailure {
			filtered = append(filtered, policy)
			continue
		}
		// process high-priority first thanks to sort above
		handlerLog := logr.FromContextOrDiscard(ctx).WithValues("policy", policy.Name, "failedNode", failedNode)
		handlerLog.Info("Startup pre-scan: performing synchronous scale-down before resume")

		// Idempotency check
		alreadyHandled := policy.Status.FailedNodeName == failedNode &&
			policy.Status.NodeFailureEventHandledAt != nil &&
			(policy.Status.NodeFailureEventDetectedAt == nil ||
				policy.Status.NodeFailureEventHandledAt.After(policy.Status.NodeFailureEventDetectedAt.Time))
		if !alreadyHandled {
			if err := (&controller.NamespaceLifecyclePolicyReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).HandleNodeFailureAtStartup(ctx, policy, failedNode); err != nil {
				handlerLog.Error(err, "Startup pre-scan: failed to handle node failure for policy")
				return resumePolicies, handledOrder, err
			}
		} else {
			handlerLog.Info("✅ Startup pre-scan: node failure already handled for this failure event — skipping")
		}
		// record even if already handled so order is known
		handledOrder = append(handledOrder, policy.Name)
		// do not add to filtered; resume suppressed
	}
	return filtered, handledOrder, nil
}

// applyStartupPolicies applies startup policies for all existing NamespaceLifecyclePolicy resources
// This runs once when the operator starts, before the controller starts processing events
// Freeze policies are processed first (by freezePriority: 0, 1, 2...), then resume policies (by startupResumePriority)
func applyStartupPolicies(ctx context.Context, mgr manager.Manager) error {
	setupLog := ctrl.Log.WithName("startup")
	startTime := time.Now()
	setupLog.Info("Applying startup policies for existing resources")

	// Create a client
	k8sClient := mgr.GetClient()

	// List all NamespaceLifecyclePolicy resources
	policyList := &appsv1alpha1.NamespaceLifecyclePolicyList{}
	if err := k8sClient.List(ctx, policyList); err != nil {
		setupLog.Error(err, "Failed to list NamespaceLifecyclePolicy resources")
		return err
	}

	setupLog.Info("Found policies", "count", len(policyList.Items))

	// Create reconciler instance to use helper functions
	reconciler := &controller.NamespaceLifecyclePolicyReconciler{
		Client: k8sClient,
		Scheme: mgr.GetScheme(),
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
	// STARTUP FREEZE: Process FREEZE policies
	// The priority chain wait is now handled autonomously by the Reconciler
	// ============================================================================
	if len(freezePolicies) > 0 {
		// sort by freezePriority so any synchronous handling respects order
		sort.Slice(freezePolicies, func(i, j int) bool {
			pi := freezePolicies[i].Spec.FreezePriority
			pj := freezePolicies[j].Spec.FreezePriority
			if pi != pj {
				return pi < pj
			}
			return freezePolicies[i].CreationTimestamp.Before(&freezePolicies[j].CreationTimestamp)
		})
		setupLog.Info("🥶 ========== STARTUP FREEZE: PROCESSING FREEZE POLICIES ==========")

		// Process all freeze policies in parallel.
		// Reconciler will enforce priority delays dynamically.
		var wg sync.WaitGroup
		for _, policy := range freezePolicies {
			wg.Add(1)
			go func(p *appsv1alpha1.NamespaceLifecyclePolicy) {
				defer wg.Done()
				policyLogger := setupLog.WithValues("policy", p.Name, "freezePriority", p.Spec.FreezePriority)
				policyLogger.Info("Triggering freeze policy", "policy", p.Name)
				if err := reconciler.ApplyStartupPolicy(ctx, p); err != nil {
					policyLogger.Error(err, "Failed to apply startup freeze policy", "policy", p.Name)
				}
			}(policy)
		}

		// Simply wait here for the trigger functions, not the operations
		wg.Wait()
		setupLog.Info("✅ ========== FREEZE POLICIES TRIGGERED ==========")
	}

	// ============================================================================
	// NODE FAILURE PRE-SCAN
	// ============================================================================
	filtered, order, err := NodeFailurePreScan(ctx, k8sClient, resumePolicies)
	if err != nil {
		setupLog.Error(err, "Startup: node failure pre-scan encountered error — continuing")
	} else {
		resumePolicies = filtered
		setupLog.V(1).Info("Startup pre-scan processed policies", "order", order)
	}



	// ============================================================================
	// STARTUP RESUME: Process RESUME policies
	// The priority chain wait is now handled autonomously by the Reconciler
	// ============================================================================
	// sort resume policies one more time before triggering (order should already be correct)
	sort.Slice(resumePolicies, func(i, j int) bool {
		pi := resumePolicies[i].Spec.StartupResumePriority
		if pi == 0 { pi = 100 }
		pj := resumePolicies[j].Spec.StartupResumePriority
		if pj == 0 { pj = 100 }
		if pi != pj { return pi < pj }
		return resumePolicies[i].CreationTimestamp.Before(&resumePolicies[j].CreationTimestamp)
	})
	if len(resumePolicies) > 0 {
		setupLog.Info("🚀 ========== STARTUP RESUME: PROCESSING RESUME POLICIES ==========")

		// Process all resume policies in parallel.
		// Reconciler will enforce priority delays dynamically.
		var wg sync.WaitGroup
		for _, policy := range resumePolicies {
			wg.Add(1)
			go func(p *appsv1alpha1.NamespaceLifecyclePolicy) {
				defer wg.Done()
				priority := p.Spec.StartupResumePriority
				if priority == 0 {
					priority = 100
				}
				policyLogger := setupLog.WithValues("policy", p.Name, "startupResumePriority", priority)
				policyLogger.Info("Triggering resume policy", "policy", p.Name)
				if err := reconciler.ApplyStartupPolicy(ctx, p); err != nil {
					policyLogger.Error(err, "Failed to apply startup resume policy", "policy", p.Name)
				}
			}(policy)
		}

		// Wait for trigger functions
		wg.Wait()

		setupLog.Info("✅ ========== RESUME POLICIES TRIGGERED ==========")
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
				// Update an annotation to trigger the reconcile
				if p.Annotations == nil {
					p.Annotations = make(map[string]string)
				}
				p.Annotations["apps.ops.dev/precondition-trigger"] = time.Now().Format(time.RFC3339)
				if err := k8sClient.Update(ctx, p); err != nil {
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

	if err := (&controller.NamespaceLifecyclePolicyReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		RESTClient: clientset.CoreV1().RESTClient(),
	}).SetupWithManager(mgr); err != nil {
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

		// Apply startup policies
		if err := applyStartupPolicies(ctx, mgr); err != nil {
			setupLog.Error(err, "Failed to apply startup policies")
		}
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
