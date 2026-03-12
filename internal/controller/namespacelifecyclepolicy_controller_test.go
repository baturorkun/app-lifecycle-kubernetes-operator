package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
)

func TestIsBlockedByHigherPriority(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1alpha1.AddToScheme(scheme)

	tests := []struct {
		name          string
		targetPolicy  *appsv1alpha1.NamespaceLifecyclePolicy
		otherPolicies []*appsv1alpha1.NamespaceLifecyclePolicy
		action        appsv1alpha1.LifecycleAction
		wantBlocked   bool
	}{
		{
			name: "resume - no higher priority policies",
			targetPolicy: &appsv1alpha1.NamespaceLifecyclePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "uid1"},
				Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
					StartupResumePriority: 50,
				},
			},
			otherPolicies: []*appsv1alpha1.NamespaceLifecyclePolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "uid2"},
					Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
						StartupResumePriority: 100, // lower priority
					},
					Status: appsv1alpha1.NamespaceLifecyclePolicyStatus{
						Phase: appsv1alpha1.PhaseResuming,
					},
				},
			},
			action:      appsv1alpha1.LifecycleActionResume,
			wantBlocked: false,
		},
		{
			name: "resume - higher priority policy is resuming",
			targetPolicy: &appsv1alpha1.NamespaceLifecyclePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "uid1"},
				Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
					StartupResumePriority: 50,
				},
			},
			otherPolicies: []*appsv1alpha1.NamespaceLifecyclePolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "uid2"},
					Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
						StartupResumePriority: 10, // higher priority
					},
					Status: appsv1alpha1.NamespaceLifecyclePolicyStatus{
						Phase: appsv1alpha1.PhaseResuming,
					},
				},
			},
			action:      appsv1alpha1.LifecycleActionResume,
			wantBlocked: true,
		},
		{
			name: "resume - higher priority policy pending startup resume",
			targetPolicy: &appsv1alpha1.NamespaceLifecyclePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "uid1"},
				Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
					StartupResumePriority: 50,
				},
			},
			otherPolicies: []*appsv1alpha1.NamespaceLifecyclePolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "uid2"},
					Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
						StartupResumePriority: 10, // higher priority
					},
					Status: appsv1alpha1.NamespaceLifecyclePolicyStatus{
						PendingStartupResume: true,
					},
				},
			},
			action:      appsv1alpha1.LifecycleActionResume,
			wantBlocked: true,
		},
		{
			name: "resume - higher priority policy is already resumed",
			targetPolicy: &appsv1alpha1.NamespaceLifecyclePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "uid1"},
				Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
					StartupResumePriority: 50,
				},
			},
			otherPolicies: []*appsv1alpha1.NamespaceLifecyclePolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "uid2"},
					Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
						StartupResumePriority: 10, // higher priority
						Action:                appsv1alpha1.LifecycleActionResume,
					},
					Status: appsv1alpha1.NamespaceLifecyclePolicyStatus{
						Phase: appsv1alpha1.PhaseResumed,
					},
				},
			},
			action:      appsv1alpha1.LifecycleActionResume,
			wantBlocked: false,
		},
		{
			name: "resume - higher priority policy with non-blocking preconditions",
			targetPolicy: &appsv1alpha1.NamespaceLifecyclePolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "p1", UID: "uid1"},
				Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
					StartupResumePriority: 50,
				},
			},
			otherPolicies: []*appsv1alpha1.NamespaceLifecyclePolicy{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "p2", UID: "uid2"},
					Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
						StartupResumePriority: 10, // higher priority
						PreConditions: &appsv1alpha1.PreConditionsConfig{
							BlockPriorityChain: func() *bool { b := false; return &b }(),
						},
					},
					Status: appsv1alpha1.NamespaceLifecyclePolicyStatus{
						Phase: appsv1alpha1.PhaseResuming,
					},
				},
			},
			action:      appsv1alpha1.LifecycleActionResume,
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{tt.targetPolicy}
			for _, p := range tt.otherPolicies {
				objs = append(objs, p)
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			reconciler := &NamespaceLifecyclePolicyReconciler{
				Client: fakeClient,
				Scheme: scheme,
			}

			blocked, _, err := reconciler.isBlockedByHigherPriority(context.Background(), tt.targetPolicy, tt.action)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantBlocked, blocked)
		})
	}
}

// TestResumeLock verifies the acquire/release semantics of the resume lock helpers.
func TestResumeLock(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &NamespaceLifecyclePolicyReconciler{Client: fakeClient, Scheme: scheme}
	ctx := context.Background()
	ns := "test-ns"

	// initial acquire should succeed
	locked, err := reconciler.acquireResumeLock(ctx, 50, ns)
	assert.NoError(t, err)
	assert.True(t, locked, "expected initial lock acquisition to succeed")

	// same or lower priority should not acquire
	locked, err = reconciler.acquireResumeLock(ctx, 50, ns)
	assert.NoError(t, err)
	assert.False(t, locked, "equal priority should be blocked")
	locked, err = reconciler.acquireResumeLock(ctx, 100, ns)
	assert.NoError(t, err)
	assert.False(t, locked, "lower priority should be blocked")

	// higher priority preempts
	locked, err = reconciler.acquireResumeLock(ctx, 10, ns)
	assert.NoError(t, err)
	assert.True(t, locked, "higher priority should preempt the lock")

	// release and then lower priority can obtain
	err = reconciler.releaseResumeLock(ctx, ns)
	assert.NoError(t, err)
	locked, err = reconciler.acquireResumeLock(ctx, 100, ns)
	assert.NoError(t, err)
	assert.True(t, locked, "lock should be available after release")
}
