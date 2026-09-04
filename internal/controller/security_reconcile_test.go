package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

func secScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		ranv1alpha1.AddToScheme, corev1.AddToScheme, networkingv1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("add to scheme: %v", err)
		}
	}
	return s
}

func enforcedCondition(p *ranv1alpha1.RANSecurityPolicy) *metav1.Condition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == "Enforced" {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// The bug: when a requested control (NetworkPolicy) fails to apply, the policy
// must NOT report a clean audit as if enforcement succeeded. It must record
// Enforced=False and surface the failure (requeue), not sit green.
func TestSecurityReconcile_NetworkPolicyApplyFailureSurfaces(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       ranv1alpha1.RANSecurityPolicySpec{NetworkIsolation: true, RuntimeMonitoring: false},
	}
	applyErr := errors.New("networkpolicy create: forbidden")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return applyErr
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}})
	if err == nil {
		t.Fatal("a failed NetworkPolicy apply must surface (requeue), not report a clean audit")
	}

	got := &ranv1alpha1.RANSecurityPolicy{}
	if e := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, got); e != nil {
		t.Fatalf("get: %v", e)
	}
	cond := enforcedCondition(got)
	if cond == nil {
		t.Fatal("status must carry an Enforced condition")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Fatalf("Enforced want False when NetworkPolicy apply failed, got %q", cond.Status)
	}
}

// The security-critical case: an isolation NetworkPolicy that EXISTS but was
// edited to be permissive must be corrected back to the required spec — a control
// that self-heals on deletion but not on weakening is trivially defeated by
// weakening. Effect-check: mutate the NP permissive, reconcile, assert restored.
func TestSecurityReconcile_NetworkPolicyDriftIsCorrected(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       ranv1alpha1.RANSecurityPolicySpec{NetworkIsolation: true, RuntimeMonitoring: false},
	}
	// A widened NetworkPolicy already in the cluster: allow-all ingress/egress,
	// no pod selector — i.e. isolation effectively disabled.
	drifted := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-fronthaul-isolation", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{}}, // {} = allow all
			Egress:      []networkingv1.NetworkPolicyEgressRule{{}},  // {} = allow all
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, drifted).
		WithStatusSubresource(policy).
		Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Effect: the NetworkPolicy spec must be restored to the required isolation.
	np := &networkingv1.NetworkPolicy{}
	if e := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-fronthaul-isolation"}, np); e != nil {
		t.Fatalf("get np: %v", e)
	}
	if len(np.Spec.Ingress) != 2 {
		t.Fatalf("drift not corrected: want 2 restrictive ingress rules, got %d (%+v)", len(np.Spec.Ingress), np.Spec.Ingress)
	}
	if len(np.Spec.Ingress[0].Ports) == 0 || np.Spec.Ingress[0].Ports[0].Port.IntValue() != 44000 {
		t.Fatalf("fronthaul port rule not restored: %+v", np.Spec.Ingress[0])
	}
	if len(np.Spec.PodSelector.MatchLabels) == 0 {
		t.Fatal("pod selector not restored — policy would still apply cluster-wide/permissively")
	}
	// Visibility: the correction must be recorded, not silent.
	got := &ranv1alpha1.RANSecurityPolicy{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, got)
	var found bool
	for _, cond := range got.Status.Conditions {
		if cond.Type == "NetworkPolicyReconciled" {
			found = true
			if cond.Reason != "DriftCorrected" {
				t.Fatalf("condition reason want DriftCorrected got %q", cond.Reason)
			}
		}
	}
	if !found {
		t.Fatal("expected a NetworkPolicyReconciled condition surfacing the correction")
	}
}

// When controls apply cleanly, Enforced=True and the reconcile requeues normally.
func TestSecurityReconcile_ControlsAppliedEnforcedTrue(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       ranv1alpha1.RANSecurityPolicySpec{NetworkIsolation: true, RuntimeMonitoring: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("clean reconcile should requeue for periodic re-audit")
	}
	got := &ranv1alpha1.RANSecurityPolicy{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, got)
	cond := enforcedCondition(got)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Enforced want True when controls applied, got %+v", cond)
	}
	// The NetworkPolicy and Falco ConfigMap should actually exist.
	np := &networkingv1.NetworkPolicy{}
	if e := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-fronthaul-isolation"}, np); e != nil {
		t.Fatalf("expected NetworkPolicy created: %v", e)
	}
	cm := &corev1.ConfigMap{}
	if e := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-falco-rules"}, cm); e != nil {
		t.Fatalf("expected Falco ConfigMap created: %v", e)
	}
}
