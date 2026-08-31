package controller

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := ranv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add to scheme: %v", err)
	}
	return s
}

// When a RANPipeline references a missing GNodeB, the reconciler records
// Phase=Failed. If persisting that status fails, the reconcile must return the
// error so controller-runtime requeues — otherwise the operator silently leaves
// the pipeline's reported phase diverged from reality (this path does not
// otherwise requeue).
func TestRANPipelineReconcile_StatusWriteFailureRequeues(t *testing.T) {
	scheme := testScheme(t)
	pipeline := &ranv1alpha1.RANPipeline{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec:       ranv1alpha1.RANPipelineSpec{GNodeBRef: "does-not-exist"},
	}

	statusErr := errors.New("status update: connection reset")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pipeline).
		WithStatusSubresource(pipeline).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
				return statusErr
			},
		}).
		Build()

	r := &RANPipelineReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "p1"}})
	if err == nil {
		t.Fatal("a failed status write on the GNodeB-not-found path must surface (requeue), not return nil")
	}
	if !errors.Is(err, statusErr) {
		t.Fatalf("want the status-update error, got %v", err)
	}
}

// Sanity: when the status write succeeds, the not-found path reconciles cleanly
// and records Phase=Failed.
func TestRANPipelineReconcile_MissingGNodeBMarksFailed(t *testing.T) {
	scheme := testScheme(t)
	pipeline := &ranv1alpha1.RANPipeline{
		ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
		Spec:       ranv1alpha1.RANPipelineSpec{GNodeBRef: "does-not-exist"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pipeline).
		WithStatusSubresource(pipeline).
		Build()

	r := &RANPipelineReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "p1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &ranv1alpha1.RANPipeline{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "p1"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != "Failed" {
		t.Fatalf("phase want Failed got %q", got.Status.Phase)
	}
}
