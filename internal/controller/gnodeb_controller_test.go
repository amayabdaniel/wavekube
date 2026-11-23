package controller

import (
	"context"
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

// The readyReplicas printcolumn (kubectl get gnodeb -> READY) must reflect the
// Deployment's actual ready count. The controller reads deploy.Status.ReadyReplicas
// to decide the phase but previously never wrote gnb.Status.ReadyReplicas, so the
// field -- and the operator-facing column built on it -- stayed at 0 no matter how
// many cell replicas were running: declared, surfaced, inert. Effect-check mid-rollout
// (3 of 5 ready): the value must be the OBSERVED 3, which also rules out a fix that
// writes the desired count (gnb.Spec.Replicas == 5) instead of the ready count.
func TestGNodeBReconcile_ReadyReplicasReflectsDeployment(t *testing.T) {
	scheme := gnbScheme(t)
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "rolling", Namespace: "default"},
		Spec:       ranv1alpha1.GNodeBSpec{Image: "nvcr.io/nvidia/aerial/aerial-ran:24.3", Replicas: 5},
	}
	// The owned Deployment already exists with 3 of 5 replicas ready. One container so
	// the reconciler's Containers[0].Image update path is valid.
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "rolling-ran", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "aerial-ran", Image: "old"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gnb, deploy).
		WithStatusSubresource(gnb).Build()
	r := &GNodeBReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "rolling", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &ranv1alpha1.GNodeB{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "rolling", Namespace: "default"}, got); err != nil {
		t.Fatalf("get gnb: %v", err)
	}
	if got.Status.ReadyReplicas != 3 {
		t.Fatalf("status.readyReplicas = %d, want 3 (the Deployment's observed ready count, not 0 and not the desired 5)", got.Status.ReadyReplicas)
	}
	// Sanity: 3 of 5 ready is still a rollout in progress, so this is genuinely the
	// mid-rollout case (not Running), which is where a desired-count fix would wrongly
	// report 5/5 complete.
	if got.Status.Phase == "Running" {
		t.Fatalf("3 of 5 ready must not be Running, got %q", got.Status.Phase)
	}
}

// A non-NotFound read error on the metrics Service must surface from Reconcile
// (so the controller requeues), not be swallowed while the reconcile goes on to
// report the cell healthy. reconcileService previously fell through a bare
// `return nil` for any error that was not IsNotFound, so an API outage or an RBAC
// denial while reading the Service left the GNodeB reporting Phase=Progressing/
// Running off a reconcile that never verified its metrics Service existed. Every
// other Get in this controller (and reconcileFalcoRules in the security one)
// surfaces non-NotFound errors; this was the lone fail-open.
func TestGNodeBReconcile_ServiceGetErrorSurfaces(t *testing.T) {
	scheme := gnbScheme(t)
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gnb", Namespace: "default"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:     "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			Replicas:  1,
			PHYConfig: ranv1alpha1.PHYConfig{Band: "n78"},
		},
	}
	getErr := errors.New("service get: forbidden") // non-NotFound: IsNotFound==false
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gnb).
		WithStatusSubresource(gnb).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				// Fail only the metrics Service read; let the GNodeB and Deployment
				// reads/creates earlier in Reconcile proceed so the failure is
				// isolated to reconcileService.
				if _, ok := obj.(*corev1.Service); ok && key.Name == "test-gnb-metrics" {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	r := &GNodeBReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-gnb", Namespace: "default"},
	})
	if err == nil {
		t.Fatal("a non-NotFound metrics Service read error must surface from Reconcile, not be swallowed")
	}

	got := &ranv1alpha1.GNodeB{}
	if e := c.Get(context.Background(), types.NamespacedName{Name: "test-gnb", Namespace: "default"}, got); e != nil {
		t.Fatalf("get gnb: %v", e)
	}
	if got.Status.Phase == "Running" {
		t.Fatalf("GNodeB must not report Running off a reconcile whose metrics Service read errored; got %q", got.Status.Phase)
	}
}

// The metrics Service must be reconciled by CONTENT, not just created when
// absent: a Service whose selector or ports were edited out-of-band (so Prometheus
// no longer discovers/scrapes the cell) must be corrected back to the required
// shape. reconcileService was previously create-only — it left an existing Service
// untouched — so this drift stood. Effect-check: seed a drifted Service, reconcile,
// assert it was restored. Mirrors TestSecurityReconcile_NetworkPolicyDriftIsCorrected.
func TestGNodeBReconcile_ServiceDriftIsCorrected(t *testing.T) {
	scheme := gnbScheme(t)
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gnb", Namespace: "default"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:     "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			Replicas:  1,
			PHYConfig: ranv1alpha1.PHYConfig{Band: "n78"},
		},
	}
	// A metrics Service that already exists but was edited out-of-band: a selector
	// that points at the wrong instance and a changed port — Prometheus would no
	// longer scrape this cell.
	drifted := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gnb-metrics", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app.kubernetes.io/instance": "someone-else"},
			Ports: []corev1.ServicePort{
				{Name: "metrics", Port: 1234, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(gnb, drifted).
		WithStatusSubresource(gnb).Build()
	r := &GNodeBReconciler{Client: c, Scheme: scheme}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test-gnb", Namespace: "default"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &corev1.Service{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "test-gnb-metrics", Namespace: "default"}, got); err != nil {
		t.Fatalf("get service: %v", err)
	}
	if got.Spec.Selector["app.kubernetes.io/instance"] != "test-gnb" {
		t.Fatalf("selector not reconciled: an out-of-band selector must be corrected back to the cell, got %v", got.Spec.Selector)
	}
	if len(got.Spec.Ports) != 1 || got.Spec.Ports[0].Port != 9090 {
		t.Fatalf("ports not reconciled: the metrics port must be restored to 9090, got %v", got.Spec.Ports)
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
