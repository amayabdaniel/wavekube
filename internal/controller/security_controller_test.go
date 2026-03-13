package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

func TestSecurityPolicy_AuditAllowedRegistries(t *testing.T) {
	r := &RANSecurityPolicyReconciler{}

	policy := &ranv1alpha1.RANSecurityPolicy{
		Spec: ranv1alpha1.RANSecurityPolicySpec{
			AllowedRegistries: []string{
				"nvcr.io/nvidia/aerial/",
				"ghcr.io/amayabdaniel/",
			},
		},
	}

	// Allowed image
	gnbAllowed := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "good-gnb"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:             "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			SecurityPolicyRef: "test-policy",
		},
	}
	violations := r.auditGNodeB(gnbAllowed, policy)
	if len(violations) != 0 {
		t.Errorf("expected 0 violations for allowed image, got %d: %v", len(violations), violations)
	}

	// Blocked image
	gnbBlocked := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-gnb"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:             "docker.io/malicious/ran:latest",
			SecurityPolicyRef: "test-policy",
		},
	}
	violations = r.auditGNodeB(gnbBlocked, policy)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation for blocked image, got %d", len(violations))
	}
	if violations[0].Rule != "AllowedRegistries" {
		t.Errorf("expected AllowedRegistries rule, got %s", violations[0].Rule)
	}
	if violations[0].Severity != "Critical" {
		t.Errorf("expected Critical severity, got %s", violations[0].Severity)
	}
}

func TestSecurityPolicy_AuditMissingPolicyRef(t *testing.T) {
	r := &RANSecurityPolicyReconciler{}

	policy := &ranv1alpha1.RANSecurityPolicy{
		Spec: ranv1alpha1.RANSecurityPolicySpec{},
	}

	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "no-ref-gnb"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image: "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			// No SecurityPolicyRef
		},
	}

	violations := r.auditGNodeB(gnb, policy)
	if len(violations) != 1 {
		t.Fatalf("expected 1 violation for missing policy ref, got %d", len(violations))
	}
	if violations[0].Rule != "SecurityPolicyRef" {
		t.Errorf("expected SecurityPolicyRef rule, got %s", violations[0].Rule)
	}
	if violations[0].Severity != "Warning" {
		t.Errorf("expected Warning severity, got %s", violations[0].Severity)
	}
}

func TestSecurityPolicy_AuditCompliant(t *testing.T) {
	r := &RANSecurityPolicyReconciler{}

	policy := &ranv1alpha1.RANSecurityPolicy{
		Spec: ranv1alpha1.RANSecurityPolicySpec{
			AllowedRegistries: []string{"nvcr.io/nvidia/"},
		},
	}

	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "compliant-gnb"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:             "nvcr.io/nvidia/aerial-ran:24.3",
			SecurityPolicyRef: "strict",
		},
	}

	violations := r.auditGNodeB(gnb, policy)
	if len(violations) != 0 {
		t.Errorf("expected fully compliant, got %d violations", len(violations))
	}
}

func TestSetCondition(t *testing.T) {
	conditions := []metav1.Condition{}

	// Add new condition
	setCondition(&conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionFalse,
		Reason: "Initializing",
	})
	if len(conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(conditions))
	}

	// Update existing condition
	setCondition(&conditions, metav1.Condition{
		Type:   "Ready",
		Status: metav1.ConditionTrue,
		Reason: "Running",
	})
	if len(conditions) != 1 {
		t.Fatalf("expected still 1 condition after update, got %d", len(conditions))
	}
	if conditions[0].Status != metav1.ConditionTrue {
		t.Errorf("expected condition status True, got %s", conditions[0].Status)
	}

	// Add different condition
	setCondition(&conditions, metav1.Condition{
		Type:   "Audited",
		Status: metav1.ConditionTrue,
		Reason: "AuditComplete",
	})
	if len(conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(conditions))
	}
}
