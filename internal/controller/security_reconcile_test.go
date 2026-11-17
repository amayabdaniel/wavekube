package controller

import (
	"context"
	"errors"
	"strings"
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

// The Falco path must fail CLOSED on a non-NotFound read error, exactly like the
// NetworkPolicy path. reconcileFalcoRules created the rules ConfigMap only when
// Get returned NotFound; any OTHER Get error (RBAC Forbidden, API timeout) fell
// through to `return nil`, so RuntimeMonitoring reported Enforced=True/
// ControlsApplied on a read that never completed — the fail-open case the rest of
// this reconciler avoids. The policy requests only RuntimeMonitoring, so the Falco
// reconcile is the sole enforcement step under test.
func TestSecurityReconcile_FalcoRulesGetErrorSurfaces(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       ranv1alpha1.RANSecurityPolicySpec{NetworkIsolation: false, RuntimeMonitoring: true},
	}
	getErr := errors.New("configmap get: forbidden") // non-NotFound: IsNotFound==false
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).
		WithStatusSubresource(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*corev1.ConfigMap); ok && key.Name == "pol-falco-rules" {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).
		Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}})
	if err == nil {
		t.Fatal("a non-NotFound Falco ConfigMap read error must surface (requeue), not report controls applied")
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
		t.Fatalf("Enforced want False when the Falco read failed, got %q", cond.Status)
	}
	if cond.Reason != "EnforcementFailed" {
		t.Fatalf("a retryable read/apply failure should name EnforcementFailed, got %q", cond.Reason)
	}
	if !strings.Contains(cond.Message, "runtime monitoring") {
		t.Errorf("Enforced message must name the failing control; got %q", cond.Message)
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

// The Falco mirror of the NetworkPolicy drift case: a rules ConfigMap that EXISTS
// but was edited to drop the real detections must be restored to the required
// ruleset. reconcileFalcoRules was create-only — it returned without touching an
// existing ConfigMap — so a weakened ruleset was never corrected, a control
// trivially defeated by editing it. Effect-check: seed a gutted ConfigMap,
// reconcile, assert the real rules are back and the correction is surfaced.
func TestSecurityReconcile_FalcoRulesDriftIsCorrected(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec:       ranv1alpha1.RANSecurityPolicySpec{NetworkIsolation: false, RuntimeMonitoring: true},
	}
	// A gutted rules ConfigMap already in the cluster: the real detections removed,
	// i.e. runtime monitoring effectively disabled while still looking present.
	drifted := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "pol-falco-rules", Namespace: "default"},
		Data:       map[string]string{"wavekube_rules.yaml": "- rule: placeholder\n  condition: never\n"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy, drifted).
		WithStatusSubresource(policy).
		Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Effect: the required rules must be restored, overwriting the gutted content.
	cm := &corev1.ConfigMap{}
	if e := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol-falco-rules"}, cm); e != nil {
		t.Fatalf("get cm: %v", e)
	}
	rules := cm.Data["wavekube_rules.yaml"]
	for _, want := range []string{"Unauthorized GPU Memory Access in RAN", "Shell Spawned in RAN Container"} {
		if !strings.Contains(rules, want) {
			t.Errorf("drift not corrected: restored ruleset must contain %q; got %q", want, rules)
		}
	}
	if strings.Contains(rules, "placeholder") {
		t.Errorf("gutted content survived: %q", rules)
	}
	// Visibility: the correction must be recorded, not silent.
	got := &ranv1alpha1.RANSecurityPolicy{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, got)
	var found bool
	for _, cond := range got.Status.Conditions {
		if cond.Type == "FalcoRulesReconciled" {
			found = true
			if cond.Reason != "DriftCorrected" {
				t.Fatalf("condition reason want DriftCorrected got %q", cond.Reason)
			}
		}
	}
	if !found {
		t.Fatal("expected a FalcoRulesReconciled condition surfacing the correction")
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
