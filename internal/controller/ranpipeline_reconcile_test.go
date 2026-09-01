package controller

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
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
		t.Fatalf("add ran scheme: %v", err)
	}
	if err := batchv1.AddToScheme(s); err != nil {
		t.Fatalf("add batch scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

// runningGNodeB and pipelineFor build the minimal objects a pipeline reconcile
// needs: a GNodeB already in phase Running (so the reconcile reaches the Job).
func runningGNodeB(name, ns string) *ranv1alpha1.GNodeB {
	return &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       ranv1alpha1.GNodeBSpec{PHYConfig: ranv1alpha1.PHYConfig{Band: "n78", Bandwidth: 100}},
		Status:     ranv1alpha1.GNodeBStatus{Phase: "Running"},
	}
}

func pipelineFor(name, ns, gnbRef string) *ranv1alpha1.RANPipeline {
	return &ranv1alpha1.RANPipeline{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       ranv1alpha1.RANPipelineSpec{GNodeBRef: gnbRef, Image: "img", PipelineDefinition: "p"},
	}
}

func readyCondFalse(t *testing.T, p *ranv1alpha1.RANPipeline) {
	t.Helper()
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == metav1.ConditionTrue {
			t.Fatalf("Ready must not be True; conditions=%+v", p.Status.Conditions)
		}
	}
}

// The bug: a pipeline whose Job has FAILED must report Phase=Failed / Ready=False,
// not "Running" just because the Job object exists.
func TestRANPipelineReconcile_FailedJobIsNotRunning(t *testing.T) {
	scheme := testScheme(t)
	gnb := runningGNodeB("g1", "default")
	pipeline := pipelineFor("p1", "default", "g1")
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-pipeline", Namespace: "default"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gnb, pipeline, failedJob).
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
	readyCondFalse(t, got)
}

// An active Job reflects as Running/Ready.
func TestRANPipelineReconcile_ActiveJobIsRunning(t *testing.T) {
	scheme := testScheme(t)
	gnb := runningGNodeB("g1", "default")
	pipeline := pipelineFor("p1", "default", "g1")
	activeJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "p1-pipeline", Namespace: "default"},
		Status:     batchv1.JobStatus{Active: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gnb, pipeline, activeJob).
		WithStatusSubresource(pipeline).
		Build()

	r := &RANPipelineReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "p1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &ranv1alpha1.RANPipeline{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "p1"}, got)
	if got.Status.Phase != "Running" {
		t.Fatalf("phase want Running got %q", got.Status.Phase)
	}
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
