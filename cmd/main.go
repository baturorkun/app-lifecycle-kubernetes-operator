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

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"github.com/go-logr/logr"
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

// processPolicyWithDelayAndResume processes a single policy: applies startup policy, waits for delay, and polls for resume completion
func processPolicyWithDelayAndResume(ctx context.Context, logger logr.Logger, reconciler *controller.NamespaceLifecyclePolicyReconciler, k8sClient client.Client, policy *appsv1alpha1.NamespaceLifecyclePolicy, priority int32) {
	logger.Info("Processing policy", "policy", policy.Name, "priority", priority)

	if err := reconciler.ApplyStartupPolicy(ctx, policy); err != nil {
		logger.Error(err, "Failed to apply startup policy", "policy", policy.Name)
		return
	}

	// Re-fetch the policy to get updated status
	latestPolicy := &appsv1alpha1.NamespaceLifecyclePolicy{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
		logger.Error(err, "Failed to re-fetch policy after ApplyStartupPolicy", "policy", policy.Name)
		return
	}

	// Wait for this policy to complete (delay + resume) - only if startupPolicy is Resume
	if latestPolicy.Spec.StartupPolicy == appsv1alpha1.StartupPolicyResume {
		logger.Info("⏳ Waiting for policy to complete", "policy", latestPolicy.Name, "priority", priority, "delay", latestPolicy.Spec.StartupResumeDelay.Duration)

		// Wait for delay to complete (if any) - use time.Sleep instead of polling
		if latestPolicy.Spec.StartupResumeDelay.Duration > 0 {
			delayDuration := latestPolicy.Spec.StartupResumeDelay.Duration
			logger.Info("⏱️ Waiting for startup resume delay", "policy", latestPolicy.Name, "delay", delayDuration)
			time.Sleep(delayDuration)
			logger.Info("✅ Resume delay completed", "policy", latestPolicy.Name)

			// Re-fetch after delay to check if resume has started
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				logger.Error(err, "Failed to re-fetch policy after delay", "policy", policy.Name)
				return
			}
		}

		// Wait for resume to complete (Phase = Resumed) - use polling
		logger.Info("⏳ Waiting for resume to complete", "policy", latestPolicy.Name)
		maxWaitTime := 30 * time.Minute // Maximum wait time
		startTime := time.Now()
		pollInterval := 2 * time.Second

		for {
			// Check timeout
			if time.Since(startTime) >= maxWaitTime {
				logger.Info("⚠️ Timeout waiting for resume to complete", "policy", latestPolicy.Name, "maxWaitTime", maxWaitTime)
				break
			}

			// Re-fetch to check status
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: policy.Name, Namespace: policy.Namespace}, latestPolicy); err != nil {
				logger.Error(err, "Failed to re-fetch policy during resume wait", "policy", policy.Name)
				break
			}

			// Check if resume is complete
			if latestPolicy.Status.Phase == appsv1alpha1.PhaseResumed {
				logger.Info("✅ Resume completed", "policy", latestPolicy.Name, "waited", time.Since(startTime))
				break
			}

			// Check if failed
			if latestPolicy.Status.Phase == appsv1alpha1.PhaseFailed {
				logger.Info("⚠️ Resume failed", "policy", latestPolicy.Name)
				break
			}

			// Sleep before checking again
			time.Sleep(pollInterval)
		}
	}
}

// applyStartupPolicies applies startup policies for all existing NamespaceLifecyclePolicy resources
// This runs once when the operator starts, before the controller starts processing events
func applyStartupPolicies(ctx context.Context, mgr manager.Manager) error {
	setupLog := ctrl.Log.WithName("startup")
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

	// Sort by priority (lower number = higher priority)
	// For same priority, sort by creation timestamp (older first)
	policies := policyList.Items
	sort.Slice(policies, func(i, j int) bool {
		// Get priority (default 100 if not specified)
		priorityI := policies[i].Spec.StartupResumePriority
		if priorityI == 0 {
			priorityI = 100
		}
		priorityJ := policies[j].Spec.StartupResumePriority
		if priorityJ == 0 {
			priorityJ = 100
		}

		// Sort by priority first
		if priorityI != priorityJ {
			return priorityI < priorityJ
		}

		// Same priority: sort by creation timestamp (older first)
		return policies[i].CreationTimestamp.Before(&policies[j].CreationTimestamp)
	})

	setupLog.Info("Policies sorted by priority", "count", len(policies))

	// Group policies by priority
	priorityGroups := make(map[int32][]*appsv1alpha1.NamespaceLifecyclePolicy)
	priorityOrder := []int32{} // To maintain order

	for i := range policies {
		policy := &policies[i]
		priority := policy.Spec.StartupResumePriority
		if priority == 0 {
			priority = 100
		}

		if _, exists := priorityGroups[priority]; !exists {
			priorityGroups[priority] = []*appsv1alpha1.NamespaceLifecyclePolicy{}
			priorityOrder = append(priorityOrder, priority)
		}
		priorityGroups[priority] = append(priorityGroups[priority], policy)
	}

	// Sort priority order (lowest number first = highest priority)
	sort.Slice(priorityOrder, func(i, j int) bool {
		return priorityOrder[i] < priorityOrder[j]
	})

	setupLog.Info("Policies grouped by priority", "priorityGroups", len(priorityOrder))

	// Process each priority group sequentially
	for _, priority := range priorityOrder {
		group := priorityGroups[priority]
		setupLog.Info("🚀 Processing priority group", "priority", priority, "policies", len(group))

		// Process all policies in this priority group in parallel
		var wg sync.WaitGroup
		for _, policy := range group {
			wg.Add(1)
			go func(p *appsv1alpha1.NamespaceLifecyclePolicy) {
				defer wg.Done()
				// Create a logger with policy name for better traceability
				policyLogger := setupLog.WithValues("policy", p.Name, "priority", priority)
				processPolicyWithDelayAndResume(ctx, policyLogger, reconciler, k8sClient, p, priority)
			}(policy)
		}

		// Wait for all policies in this priority group to complete
		wg.Wait()
		setupLog.Info("✅ Priority group completed", "priority", priority, "policies", len(group))
	}

	setupLog.Info("Startup policy check completed")
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
