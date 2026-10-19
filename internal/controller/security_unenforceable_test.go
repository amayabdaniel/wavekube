package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

// The defect: four spec fields (imageScanningEnabled, maxCVSSScore,
// seccompProfile, encryptFronthaul) are declared on RANSecurityPolicySpec and
// read by no code. An operator could set them, get Compliant=true and
// Enforced=True/"Requested security controls applied", and believe controls were
// in force that nothing ever looked at. Declared-and-inert is worse than absent,
// because absent cannot be mistaken for applied.
func TestSecurityReconcile_UnimplementedControlsAreNotReportedApplied(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec: ranv1alpha1.RANSecurityPolicySpec{
			SeccompProfile:   "runtime/default",
			EncryptFronthaul: true,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).WithStatusSubresource(policy).Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}})

	// A missing implementation is permanent. Returning an error would hot-loop
	// with backoff against a gap only new code can close, so it must not.
	if err != nil {
		t.Fatalf("an unimplemented control is not retryable and must not error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("should still requeue on the normal re-audit cadence")
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
		t.Fatalf("Enforced want False when requested controls have no implementation, got %q", cond.Status)
	}
	if cond.Reason != "ControlsUnimplemented" {
		t.Fatalf("Reason want ControlsUnimplemented, got %q", cond.Reason)
	}
	// The operator has to be told WHICH controls, or the condition is unactionable.
	for _, want := range []string{"seccompProfile", "encryptFronthaul"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("Enforced message must name the unenforceable control %q; got %q", want, cond.Message)
		}
	}
	// No GNodeBs exist, so the audit found no violations — but compliance cannot be
	// asserted over controls that were never evaluated.
	if len(got.Status.Violations) != 0 {
		t.Fatalf("precondition: expected no audit violations, got %d", len(got.Status.Violations))
	}
	if got.Status.Compliant {
		t.Fatal("Compliant must not be true while requested controls were never evaluated")
	}
}

// An unimplemented control must not drown out a retryable apply failure: the
// apply failure is the one that still needs backoff.
func TestSecurityReconcile_UnimplementedControlDoesNotMaskApplyFailure(t *testing.T) {
	scheme := secScheme(t)
	policy := &ranv1alpha1.RANSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "pol", Namespace: "default"},
		Spec: ranv1alpha1.RANSecurityPolicySpec{
			NetworkIsolation: true,
			SeccompProfile:   "runtime/default",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(policy).WithStatusSubresource(policy).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errors.New("networkpolicy create: forbidden")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	r := &RANSecurityPolicyReconciler{Client: c, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pol"}})
	if err == nil {
		t.Fatal("a retryable apply failure must still surface as an error")
	}

	got := &ranv1alpha1.RANSecurityPolicy{}
	_ = c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "pol"}, got)
	cond := enforcedCondition(got)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("Enforced want False, got %+v", cond)
	}
	if cond.Reason != "EnforcementFailed" {
		t.Fatalf("the retryable failure should name the reason, want EnforcementFailed got %q", cond.Reason)
	}
	// Both problems must remain visible; neither hides the other.
	for _, want := range []string{"network isolation", "seccompProfile"} {
		if !strings.Contains(cond.Message, want) {
			t.Errorf("message must retain %q; got %q", want, cond.Message)
		}
	}
}

func TestUnenforceableControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec ranv1alpha1.RANSecurityPolicySpec
		want []string
	}{
		{"none requested", ranv1alpha1.RANSecurityPolicySpec{}, nil},
		{"implemented controls are not reported", ranv1alpha1.RANSecurityPolicySpec{
			NetworkIsolation: true, RuntimeMonitoring: true, AllowedRegistries: []string{"nvcr.io/"},
		}, nil},
		{"image scanning", ranv1alpha1.RANSecurityPolicySpec{ImageScanningEnabled: true}, []string{"imageScanningEnabled"}},
		{"cvss threshold", ranv1alpha1.RANSecurityPolicySpec{MaxCVSSScore: 7.0}, []string{"maxCVSSScore"}},
		{"seccomp", ranv1alpha1.RANSecurityPolicySpec{SeccompProfile: "runtime/default"}, []string{"seccompProfile"}},
		{"fronthaul encryption", ranv1alpha1.RANSecurityPolicySpec{EncryptFronthaul: true}, []string{"encryptFronthaul"}},
		{"all four", ranv1alpha1.RANSecurityPolicySpec{
			ImageScanningEnabled: true, MaxCVSSScore: 1, SeccompProfile: "x", EncryptFronthaul: true,
		}, []string{"imageScanningEnabled", "maxCVSSScore", "seccompProfile", "encryptFronthaul"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unenforceableControls(tc.spec); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

// Guard against the defect recurring. Every field of RANSecurityPolicySpec must
// be either implemented by this controller or reported by unenforceableControls.
// A new field that is neither is the declared-and-inert defect again: settable,
// never read, and reported as applied. This test fails until it is classified.
func TestRANSecurityPolicySpec_EveryFieldIsClassified(t *testing.T) {
	// Controls this controller actually acts on. Each is read in Reconcile or
	// auditGNodeB; moving one here without an implementation defeats the guard.
	implemented := map[string]string{
		"NetworkIsolation":  "reconcileNetworkPolicies",
		"RuntimeMonitoring": "reconcileFalcoRules",
		"AllowedRegistries": "auditGNodeB",
	}
	// Fields unenforceableControls reports when set, verified by exercising it
	// rather than by trusting this list.
	reported := map[string]ranv1alpha1.RANSecurityPolicySpec{
		"ImageScanningEnabled": {ImageScanningEnabled: true},
		"MaxCVSSScore":         {MaxCVSSScore: 1},
		"SeccompProfile":       {SeccompProfile: "x"},
		"EncryptFronthaul":     {EncryptFronthaul: true},
	}
	for field, spec := range reported {
		if len(unenforceableControls(spec)) == 0 {
			t.Errorf("%q is listed as reported-unenforceable but unenforceableControls ignores it", field)
		}
	}

	tp := reflect.TypeOf(ranv1alpha1.RANSecurityPolicySpec{})
	for i := 0; i < tp.NumField(); i++ {
		name := tp.Field(i).Name
		_, impl := implemented[name]
		_, rep := reported[name]
		if impl && rep {
			t.Errorf("%q is classified both implemented and unenforceable; pick one", name)
		}
		if !impl && !rep {
			t.Errorf("RANSecurityPolicySpec.%s is neither implemented nor reported as unenforceable: "+
				"an operator can set it, read Enforced=True, and believe a control is in force that "+
				"no code reads. Implement it, or add it to unenforceableControls.", name)
		}
	}
}
