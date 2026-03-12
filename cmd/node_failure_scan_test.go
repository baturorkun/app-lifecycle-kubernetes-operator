package main

import (
	"context"
	"testing"

	appsapi "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1alpha1 "github.com/baturorkun/app-lifecycle-kubernetes-operator/api/v1alpha1"
)

func TestNodeFailurePreScanSortsAndHandles(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = appsv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	// needed for deployment/statefulset listing in HandleNodeFailureAtStartup
	_ = appsapi.AddToScheme(scheme)

	// create two policies in "wrong" order (low priority first)
	low := &appsv1alpha1.NamespaceLifecyclePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "low", Namespace: "default", UID: "uid-low"},
		Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
			StartupResumePriority: 20,
			HandleNodeFailure:     true,
		},
	}
	high := &appsv1alpha1.NamespaceLifecyclePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "high", Namespace: "default", UID: "uid-high"},
		Spec: appsv1alpha1.NamespaceLifecyclePolicySpec{
			StartupResumePriority: 10,
			HandleNodeFailure:     true,
		},
	}

	// Node that is NotReady
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "badnode"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionFalse,
			}},
		},
	}

	// build fake client containing objects in unsorted order
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1alpha1.NamespaceLifecyclePolicy{}).WithObjects(low, high, node).Build()

	ctx := context.Background()
	filtered, order, err := NodeFailurePreScan(ctx, fakeClient, []*appsv1alpha1.NamespaceLifecyclePolicy{low, high})
	if err != nil {
		t.Fatalf("NodeFailurePreScan returned error: %v", err)
	}
	if err != nil {
		t.Fatalf("NodeFailurePreScan returned error: %v", err)
	}

	// both had handleNodeFailure so filtered slice should be empty
	if len(filtered) != 0 {
		t.Errorf("expected no policies returned, got %d", len(filtered))
	}

	// order must list high before low
	if len(order) != 2 || order[0] != "high" || order[1] != "low" {
		t.Errorf("unexpected handling order: %v", order)
	}

	// verify statuses were updated (PendingStartupResume true)
	pol := &appsv1alpha1.NamespaceLifecyclePolicy{}
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: "high", Namespace: "default"}, pol); err != nil {
		t.Fatalf("failed to fetch high policy: %v", err)
	}
	if !pol.Status.PendingStartupResume {
		t.Error("high priority policy should have PendingStartupResume=true")
	}
	if err := fakeClient.Get(ctx, client.ObjectKey{Name: "low", Namespace: "default"}, pol); err != nil {
		t.Fatalf("failed to fetch low policy: %v", err)
	}
	if !pol.Status.PendingStartupResume {
		t.Error("low priority policy should have PendingStartupResume=true")
	}
}
