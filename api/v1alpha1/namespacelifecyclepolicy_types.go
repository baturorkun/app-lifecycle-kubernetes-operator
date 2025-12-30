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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// LifecycleAction defines the lifecycle operation action
// +kubebuilder:validation:Enum=Freeze;Resume
type LifecycleAction string

const (
	// LifecycleActionFreeze indicates that the namespace should be frozen
	LifecycleActionFreeze LifecycleAction = "Freeze"

	// LifecycleActionResume indicates that the namespace should be resumed
	LifecycleActionResume LifecycleAction = "Resume"
)

// StartupPolicy defines the startup behavior policy
// +kubebuilder:validation:Enum=Ignore;Resume;Freeze
type StartupPolicy string

const (
	// StartupPolicyIgnore indicates no action should be taken at startup
	StartupPolicyIgnore StartupPolicy = "Ignore"

	// StartupPolicyResume indicates the namespace should resume at startup
	StartupPolicyResume StartupPolicy = "Resume"

	// StartupPolicyFreeze indicates the namespace should be frozen at startup
	StartupPolicyFreeze StartupPolicy = "Freeze"
)

// Phase defines the current phase of the namespace lifecycle operation
// +kubebuilder:validation:Enum=Idle;Freezing;Frozen;Resuming;Resumed;Failed
type Phase string

const (
	// PhaseIdle indicates no operation is in progress
	PhaseIdle Phase = "Idle"

	// PhaseFreezing indicates the namespace is being frozen
	PhaseFreezing Phase = "Freezing"

	// PhaseFrozen indicates the namespace is frozen
	PhaseFrozen Phase = "Frozen"

	// PhaseResuming indicates the namespace is being resumed
	PhaseResuming Phase = "Resuming"

	// PhaseResumed indicates the namespace has been successfully resumed
	PhaseResumed Phase = "Resumed"

	// PhaseFailed indicates the operation has failed
	PhaseFailed Phase = "Failed"

	// AnnotationOriginalReplicas stores the original replica count before freezing
	AnnotationOriginalReplicas = "apps.ops.dev/original-replicas"

	// AnnotationOriginalTerminationGracePeriod stores the original terminationGracePeriodSeconds before freezing
	AnnotationOriginalTerminationGracePeriod = "apps.ops.dev/original-termination-grace-period"
)

// NamespaceLifecyclePolicySpec defines the desired state of NamespaceLifecyclePolicy
type NamespaceLifecyclePolicySpec struct {
	// INSERT ADDITIONAL SPEC FIELDS - desired state of cluster
	// Important: Run "make" to regenerate code after modifying this file
	// The following markers will use OpenAPI v3 schema to validate the value
	// More info: https://book.kubebuilder.io/reference/markers/crd-validation.html

	// targetNamespace specifies the name of the namespace to which this policy applies.
	// This field is required and must reference an existing namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	TargetNamespace string `json:"targetNamespace"`

	// selector is a label selector to filter which Deployments and StatefulSets
	// in the target namespace should be affected by this policy.
	// If not specified, all Deployments and StatefulSets in the namespace will be affected.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// action defines the lifecycle operation to perform on the target namespace.
	// Valid values are:
	// - "Freeze": Prevents new deployments and modifications in the namespace
	// - "Resume": Allows normal operations in the namespace
	// This field is required.
	// +kubebuilder:validation:Required
	Action LifecycleAction `json:"action"`

	// operationId is an optional identifier for this operation.
	// Can be used for tracking and correlation purposes.
	// +optional
	OperationId string `json:"operationId,omitempty"`

	// startupPolicy defines the behavior when the operator starts.
	// Valid values are:
	// - "Ignore": No action taken at startup
	// - "Resume": Resume the namespace at startup
	// - "Freeze": Freeze the namespace at startup
	// This field is required.
	// +kubebuilder:validation:Required
	StartupPolicy StartupPolicy `json:"startupPolicy"`

	// balancePods enables automatic pod redistribution when new nodes become Ready
	// after a Resume operation. Works in conjunction with balanceWindowSeconds.
	// When enabled, the operator watches for node Ready events and triggers rolling
	// restarts to redistribute pods across all available nodes.
	// Only applies when action is Resume.
	// +optional
	BalancePods bool `json:"balancePods,omitempty"`

	// balanceWindowSeconds defines the time window (in seconds) after Resume
	// during which the operator will automatically trigger rolling restarts
	// when new nodes become Ready. This ensures balanced pod distribution.
	// Only used when balancePods is true.
	// Default: 600 (10 minutes)
	// +optional
	// +kubebuilder:default=600
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	BalanceWindowSeconds int32 `json:"balanceWindowSeconds,omitempty"`

	// terminationGracePeriodSeconds defines graceful shutdown settings for different resource types.
	// +optional
	TerminationGracePeriodSeconds *TerminationGracePeriodConfig `json:"terminationGracePeriodSeconds,omitempty"`

	// startupNodeReadinessPolicy defines node readiness requirements before
	// applying startup policy. Only applies when startupPolicy is Resume or Freeze.
	// +optional
	StartupNodeReadinessPolicy *StartupNodeReadinessPolicy `json:"startupNodeReadinessPolicy,omitempty"`

	// resumeDelay specifies how long to wait before starting a Resume operation.
	// ONLY applies to Resume operations (action: Resume OR startupPolicy: Resume).
	// Does NOT apply to Freeze operations.
	// Useful for staggering multiple namespace resume operations to prevent
	// simultaneous resume bursts that could overload nodes.
	// Default: 0s (no delay)
	// +optional
	// +kubebuilder:default="0s"
	ResumeDelay metav1.Duration `json:"resumeDelay,omitempty"`

	// adaptiveThrottling enables adaptive throttling during Resume operations
	// to prevent node overload by monitoring node conditions and pending pods.
	// Only applies when action is Resume.
	// +optional
	AdaptiveThrottling *AdaptiveThrottlingConfig `json:"adaptiveThrottling,omitempty"`
}

// TerminationGracePeriodConfig defines terminationGracePeriodSeconds for different resource types.
type TerminationGracePeriodConfig struct {
	// deployment specifies the terminationGracePeriodSeconds for Deployments.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	Deployment *int64 `json:"deployment,omitempty"`

	// statefulSet specifies the terminationGracePeriodSeconds for StatefulSets.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	StatefulSet *int64 `json:"statefulSet,omitempty"`
}

// StartupNodeReadinessPolicy defines node readiness requirements for startup actions
type StartupNodeReadinessPolicy struct {
	// enabled activates node readiness checking before applying startup policy
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// timeoutSeconds is the maximum time to wait for nodes to become ready
	// After timeout, startup action proceeds with available nodes
	// Default: 60
	// +optional
	// +kubebuilder:default=60
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=600
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// minReadyNodes is the minimum number of ready worker nodes required
	// before applying startup action
	// Only used when requireAllNodes is false
	// Default: 1
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	MinReadyNodes int32 `json:"minReadyNodes,omitempty"`

	// requireAllNodes when true, waits for ALL worker nodes matching nodeSelector to be ready
	// When false, uses minReadyNodes instead
	// This field is required - you must explicitly choose the behavior
	// +kubebuilder:validation:Required
	RequireAllNodes bool `json:"requireAllNodes"`

	// nodeSelector selects which nodes to count as workers
	// Default: {"node-role.kubernetes.io/worker": ""}
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// AdaptiveThrottlingConfig defines adaptive throttling configuration for Resume operations
type AdaptiveThrottlingConfig struct {
	// enabled activates adaptive throttling during Resume operations
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// initialBatchSize is the starting number of workloads to resume in each batch
	// Default: 3
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=20
	InitialBatchSize int32 `json:"initialBatchSize,omitempty"`

	// minBatchSize is the minimum batch size when throttling
	// Default: 1
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MinBatchSize int32 `json:"minBatchSize,omitempty"`

	// batchInterval is the wait time between batches in seconds
	// Default: 5
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=60
	BatchInterval int32 `json:"batchInterval,omitempty"`

	// signalChecks defines which signals to monitor and how to respond
	// +optional
	SignalChecks *SignalChecksConfig `json:"signalChecks,omitempty"`

	// nodeSelector selects which nodes to monitor for signals
	// Default: {"node-role.kubernetes.io/worker": ""}
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// fallbackOnMetricsUnavailable when true, continues resume without throttling
	// if node metrics or signals cannot be collected
	// Default: true
	// +optional
	// +kubebuilder:default=true
	FallbackOnMetricsUnavailable bool `json:"fallbackOnMetricsUnavailable,omitempty"`
}

// SignalChecksConfig defines signal monitoring configuration
type SignalChecksConfig struct {
	// checkNodeReady enables monitoring of node Ready status
	// +optional
	CheckNodeReady *NodeReadyCheckConfig `json:"checkNodeReady,omitempty"`

	// checkNodePressure enables monitoring of node pressure conditions
	// +optional
	CheckNodePressure *NodePressureCheckConfig `json:"checkNodePressure,omitempty"`

	// checkPendingPods enables monitoring of pending pods
	// +optional
	CheckPendingPods *PendingPodsCheckConfig `json:"checkPendingPods,omitempty"`
}

// NodeReadyCheckConfig defines configuration for node Ready status monitoring
type NodeReadyCheckConfig struct {
	// enabled activates node Ready status checking
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// waitInterval is how often to check node status when waiting (in seconds)
	// Default: 20
	// +optional
	// +kubebuilder:default=20
	// +kubebuilder:validation:Minimum=5
	// +kubebuilder:validation:Maximum=300
	WaitInterval int32 `json:"waitInterval,omitempty"`

	// maxWaitTime is the maximum time to wait for nodes to become ready (in seconds)
	// Default: 1800 (30 minutes)
	// +optional
	// +kubebuilder:default=1800
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=7200
	MaxWaitTime int32 `json:"maxWaitTime,omitempty"`
}

// NodePressureCheckConfig defines configuration for node pressure monitoring
type NodePressureCheckConfig struct {
	// enabled activates node pressure checking
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// pressureTypes lists which pressure conditions to monitor
	// Valid values: MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable
	// +optional
	PressureTypes []string `json:"pressureTypes,omitempty"`

	// slowdownPercent is the percentage of initial batch size to use when pressure detected
	// For example, 50 means reduce batch size to 50% of initial value
	// Default: 50
	// +optional
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=100
	SlowdownPercent int32 `json:"slowdownPercent,omitempty"`
}

// PendingPodsCheckConfig defines configuration for pending pods monitoring
type PendingPodsCheckConfig struct {
	// enabled activates pending pods checking
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// threshold is the number of pending pods that triggers throttling
	// Default: 5
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Threshold int32 `json:"threshold,omitempty"`

	// slowdownPercent is the percentage of current batch size to use when pending pods exceed threshold
	// For example, 70 means reduce batch size to 70% of current value
	// Default: 70
	// +optional
	// +kubebuilder:default=70
	// +kubebuilder:validation:Minimum=10
	// +kubebuilder:validation:Maximum=100
	SlowdownPercent int32 `json:"slowdownPercent,omitempty"`
}

// NamespaceLifecyclePolicyStatus defines the observed state of NamespaceLifecyclePolicy.
type NamespaceLifecyclePolicyStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// phase represents the current phase of the namespace lifecycle operation.
	// Possible values are: Idle, Freezing, Frozen, Resuming, Active, Failed
	// +optional
	Phase Phase `json:"phase,omitempty"`

	// lastHandledOperationId stores the operation ID of the last successfully handled operation.
	// This helps track which operations have been processed by the controller.
	// +optional
	LastHandledOperationId string `json:"lastHandledOperationId,omitempty"`

	// message provides a human-readable message about the current status.
	// This field typically contains details about the current operation or any errors encountered.
	// +optional
	Message string `json:"message,omitempty"`

	// lastStartupAt stores the timestamp when startup policy was last checked
	// This is updated every time the operator starts and evaluates the startup policy
	// +optional
	LastStartupAt *metav1.Time `json:"lastStartupAt,omitempty"`

	// lastStartupAction records the action taken during the last startup policy check
	// Possible values:
	// - FREEZE_APPLIED - Startup policy froze the namespace
	// - RESUME_APPLIED - Startup policy resumed the namespace
	// - NO_ACTION_ALREADY_FROZEN - Already frozen, no action needed
	// - NO_ACTION_ALREADY_RESUMED - Already resumed, no action needed
	// - SKIPPED_IGNORE - StartupPolicy is set to Ignore
	// - SKIPPED_NAMESPACE_NOT_FOUND - Target namespace doesn't exist
	// +optional
	LastStartupAction string `json:"lastStartupAction,omitempty"`

	// lastResumeAt stores the timestamp when the last Resume operation completed.
	// Used to determine if automatic pod balancing should still be active.
	// Only set when action is Resume and the operation completes successfully.
	// +optional
	LastResumeAt *metav1.Time `json:"lastResumeAt,omitempty"`

	// startupNodesWaited records how many seconds we waited for nodes during startup
	// +optional
	StartupNodesWaited *int32 `json:"startupNodesWaited,omitempty"`

	// startupReadyNodes records how many nodes were ready when startup action was applied
	// +optional
	StartupReadyNodes *int32 `json:"startupReadyNodes,omitempty"`

	// adaptiveProgress tracks progress of adaptive throttling during Resume operations
	// Only populated when adaptiveThrottling is enabled
	// +optional
	AdaptiveProgress *AdaptiveProgressStatus `json:"adaptiveProgress,omitempty"`

	// conditions represent the current state of the NamespaceLifecyclePolicy resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AdaptiveProgressStatus tracks the progress of adaptive throttling during Resume
type AdaptiveProgressStatus struct {
	// totalWorkloads is the total number of workloads to resume
	// +optional
	TotalWorkloads int32 `json:"totalWorkloads,omitempty"`

	// resumedWorkloads is the number of workloads successfully resumed so far
	// +optional
	ResumedWorkloads int32 `json:"resumedWorkloads,omitempty"`

	// currentBatchSize is the current batch size being used (dynamically adjusted)
	// +optional
	CurrentBatchSize int32 `json:"currentBatchSize,omitempty"`

	// activeSignals lists the currently active signals affecting throttling
	// +optional
	ActiveSignals []Signal `json:"activeSignals,omitempty"`

	// lastCheckTime is when signals were last checked
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`

	// message provides human-readable progress information
	// +optional
	Message string `json:"message,omitempty"`
}

// SignalType defines the type of signal detected
// +kubebuilder:validation:Enum=NodeNotReady;NodePressure;PendingPods
type SignalType string

const (
	// SignalNodeNotReady indicates one or more nodes are NotReady
	SignalNodeNotReady SignalType = "NodeNotReady"

	// SignalNodePressure indicates node pressure conditions detected
	SignalNodePressure SignalType = "NodePressure"

	// SignalPendingPods indicates too many pods are pending
	SignalPendingPods SignalType = "PendingPods"
)

// SignalSeverity defines the severity level of a signal
// +kubebuilder:validation:Enum=Critical;Warning;Info
type SignalSeverity string

const (
	// SignalSeverityCritical indicates resume should stop
	SignalSeverityCritical SignalSeverity = "Critical"

	// SignalSeverityWarning indicates resume should slow down significantly
	SignalSeverityWarning SignalSeverity = "Warning"

	// SignalSeverityInfo indicates resume should slow down slightly
	SignalSeverityInfo SignalSeverity = "Info"
)

// Signal represents a detected condition that affects throttling
type Signal struct {
	// type is the type of signal
	// +optional
	Type SignalType `json:"type,omitempty"`

	// severity indicates how critical the signal is
	// +optional
	Severity SignalSeverity `json:"severity,omitempty"`

	// node is the name of the affected node (for node-related signals)
	// +optional
	Node string `json:"node,omitempty"`

	// condition is the specific condition type (for pressure signals)
	// +optional
	Condition string `json:"condition,omitempty"`

	// count is a numeric value associated with the signal (e.g., pending pod count)
	// +optional
	Count int32 `json:"count,omitempty"`

	// message provides human-readable details about the signal
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// NamespaceLifecyclePolicy is the Schema for the namespacelifecyclepolicies API
type NamespaceLifecyclePolicy struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of NamespaceLifecyclePolicy
	// +required
	Spec NamespaceLifecyclePolicySpec `json:"spec"`

	// status defines the observed state of NamespaceLifecyclePolicy
	// +optional
	Status NamespaceLifecyclePolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NamespaceLifecyclePolicyList contains a list of NamespaceLifecyclePolicy
type NamespaceLifecyclePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NamespaceLifecyclePolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&NamespaceLifecyclePolicy{}, &NamespaceLifecyclePolicyList{})
}
