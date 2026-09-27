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
// (named <gnb>-ran) via the reconciler and reflect Running in status. The prior
// version of this test set spec fields on a literal and asserted them back
// without ever calling Reconcile — it could not fail regardless of controller
// behaviour; this exercises the real reconcile path.
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

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-gnb", Namespace: "default"},
	}); err != nil {
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
	if got.Status.Phase != "Running" {
		t.Errorf("status phase = %q, want Running", got.Status.Phase)
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
