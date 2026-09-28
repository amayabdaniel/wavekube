package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

func gnbScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		ranv1alpha1.AddToScheme, appsv1.AddToScheme, corev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}
	return s
}

// Effect check: reconciling a GNodeB must actually CREATE the RAN Deployment
// (named <gnb>-ran) via the reconciler. A freshly created Deployment has no ready
// replicas yet, so the honest phase is Progressing (not Running) and the reconcile
// requeues to converge. The prior version of this test set spec fields on a
// literal and asserted them back without ever calling Reconcile.
func TestGNodeBReconcile_CreatesDeployment(t *testing.T) {
	scheme := gnbScheme(t)
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gnb", Namespace: "default"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:        "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			Replicas:     1,
			GPUResources: ranv1alpha1.GPUResourceSpec{Count: 1, Type: "A100", EnableRDMA: false},
			PHYConfig:    ranv1alpha1.PHYConfig{Bandwidth: 100, Numerology: 1, Band: "n78", MaxUEs: 32},
			Network:      ranv1alpha1.NetworkConfig{FronthaulInterface: "eth1"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gnb).WithStatusSubresource(gnb).Build()
	r := &GNodeBReconciler{Client: c, Scheme: scheme}

	res, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-gnb", Namespace: "default"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// The reconciler must have created the Deployment (not the test).
	deploy := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gnb-ran", Namespace: "default"}, deploy); err != nil {
		t.Fatalf("reconciler did not create the RAN Deployment: %v", err)
	}
	if got := deploy.Spec.Template.Spec.Containers[0].Image; got != gnb.Spec.Image {
		t.Errorf("deployment image = %q, want %q", got, gnb.Spec.Image)
	}
	got := &ranv1alpha1.GNodeB{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gnb", Namespace: "default"}, got); err != nil {
		t.Fatalf("get gnb: %v", err)
	}
	if got.Status.Phase != "Progressing" {
		t.Errorf("status phase = %q, want Progressing (0 replicas ready on a fresh create)", got.Status.Phase)
	}
	if res.RequeueAfter == 0 {
		t.Error("a progressing GNodeB should requeue to converge, got no requeue")
	}
}

// The wrong-STATE fix: a GNodeB must NOT report Running while its Deployment has
// 0/N replicas ready — the controller previously set Phase=Running the moment the
// Deployment object existed, so an operator debugging a CrashLooping cell saw a
// false "healthy". Running only when ReadyReplicas == desired.
func TestGNodeBReconcile_RunningOnlyWhenReplicasReady(t *testing.T) {
	scheme := gnbScheme(t)
	newGNB := func(name string, replicas int32) *ranv1alpha1.GNodeB {
		return &ranv1alpha1.GNodeB{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       ranv1alpha1.GNodeBSpec{Image: "nvcr.io/nvidia/aerial/aerial-ran:24.3", Replicas: replicas},
		}
	}
	// One container so the reconciler's spec-update path (Containers[0].Image) is valid.
	withContainer := appsv1.DeploymentSpec{
		Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "aerial-ran", Image: "old"}}},
		},
	}
	// A Deployment that already exists but has 0/2 ready.
	notReady := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "cold-ran", Namespace: "default"},
		Spec:       withContainer,
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 0},
	}
	// A Deployment with all replicas ready.
	ready := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "hot-ran", Namespace: "default"},
		Spec:       withContainer,
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 2},
	}

	cold := newGNB("cold", 2)
	hot := newGNB("hot", 2)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(cold, hot, notReady, ready).
		WithStatusSubresource(cold, hot).Build()
	r := &GNodeBReconciler{Client: c, Scheme: scheme}

	// 0/2 ready → must NOT be Running, and must requeue.
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cold", Namespace: "default"}})
	if err != nil {
		t.Fatalf("reconcile cold: %v", err)
	}
	got := &ranv1alpha1.GNodeB{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cold", Namespace: "default"}, got)
	if got.Status.Phase == "Running" {
		t.Fatalf("GNodeB with 0/2 ready replicas must not report Running")
	}
	if res.RequeueAfter == 0 {
		t.Error("a not-ready GNodeB should requeue")
	}

	// 2/2 ready → Running, no requeue.
	res, err = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "hot", Namespace: "default"}})
	if err != nil {
		t.Fatalf("reconcile hot: %v", err)
	}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "hot", Namespace: "default"}, got)
	if got.Status.Phase != "Running" {
		t.Fatalf("GNodeB with 2/2 ready replicas should be Running, got %q", got.Status.Phase)
	}
	if res.RequeueAfter != 0 {
		t.Errorf("a ready GNodeB should not requeue on a timer, got %v", res.RequeueAfter)
	}
}

func TestGNodeBReconciler_BuildDeployment(t *testing.T) {
	r := &GNodeBReconciler{}
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prod-gnb",
			Namespace: "telecom",
		},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:    "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			Replicas: 2,
			GPUResources: ranv1alpha1.GPUResourceSpec{
				Count:      2,
				Type:       "H100",
				EnableRDMA: true,
			},
			PHYConfig: ranv1alpha1.PHYConfig{
				Bandwidth:  100,
				Numerology: 1,
				Band:       "n77",
				MaxUEs:     64,
			},
			Network: ranv1alpha1.NetworkConfig{
				FronthaulInterface: "eth2",
				MidhaulCIDR:        "10.10.0.0/24",
				BackhaulCIDR:       "10.20.0.0/24",
			},
		},
	}

	deploy := r.buildDeployment(gnb)

	if deploy.Name != "prod-gnb-ran" {
		t.Errorf("expected deployment name prod-gnb-ran, got %s", deploy.Name)
	}
	if deploy.Namespace != "telecom" {
		t.Errorf("expected namespace telecom, got %s", deploy.Namespace)
	}
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("expected 2 replicas, got %d", *deploy.Spec.Replicas)
	}
	if !deploy.Spec.Template.Spec.HostNetwork {
		t.Error("expected HostNetwork=true when RDMA is enabled")
	}

	container := deploy.Spec.Template.Spec.Containers[0]
	if container.Image != "nvcr.io/nvidia/aerial/aerial-ran:24.3" {
		t.Errorf("expected aerial image, got %s", container.Image)
	}

	gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
	if gpuLimit.Value() != 2 {
		t.Errorf("expected 2 GPU limit, got %d", gpuLimit.Value())
	}

	// Check env vars
	envMap := make(map[string]string)
	for _, env := range container.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["AERIAL_PHY_BAND"] != "n77" {
		t.Errorf("expected band n77 in env, got %s", envMap["AERIAL_PHY_BAND"])
	}
	if envMap["AERIAL_FRONTHAUL_IFACE"] != "eth2" {
		t.Errorf("expected eth2 fronthaul, got %s", envMap["AERIAL_FRONTHAUL_IFACE"])
	}

	// Check node selector
	ns := deploy.Spec.Template.Spec.NodeSelector
	if ns["nvidia.com/gpu.product"] != "H100" {
		t.Errorf("expected H100 node selector, got %s", ns["nvidia.com/gpu.product"])
	}
}

// checkSecurityPolicy must reject an image whose registry is not in the
// referenced RANSecurityPolicy's AllowedRegistries, and pass one that is. The
// prior version asserted a spec field against the constant it was just set to
// and never invoked the policy check at all.
func TestGNodeB_checkSecurityPolicy_EnforcesAllowedRegistries(t *testing.T) {
	scheme := gnbScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "strict-policy", Namespace: "default"},
		Spec:       ranv1alpha1.RANSecurityPolicySpec{AllowedRegistries: []string{"nvcr.io/nvidia/"}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).Build()
	r := &GNodeBReconciler{Client: c, Scheme: scheme}

	bad := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "default"},
		Spec:       ranv1alpha1.GNodeBSpec{Image: "evil.example/aerial:latest", SecurityPolicyRef: "strict-policy"},
	}
	if err := r.checkSecurityPolicy(context.Background(), bad); err == nil {
		t.Fatal("expected a violation for an image outside the allowed registries, got nil")
	}

	ok := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "g2", Namespace: "default"},
		Spec:       ranv1alpha1.GNodeBSpec{Image: "nvcr.io/nvidia/aerial/aerial-ran:24.3", SecurityPolicyRef: "strict-policy"},
	}
	if err := r.checkSecurityPolicy(context.Background(), ok); err != nil {
		t.Fatalf("allowed-registry image should pass, got %v", err)
	}
}
