package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

type RANSecurityPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ran.wavekube.io,resources=ransecuritypolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ran.wavekube.io,resources=ransecuritypolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *RANSecurityPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	policy := &ranv1alpha1.RANSecurityPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling RANSecurityPolicy", "name", policy.Name)

	var violations []ranv1alpha1.SecurityViolation

	// Audit all GNodeBs in this namespace against the policy
	gnbList := &ranv1alpha1.GNodeBList{}
	if err := r.List(ctx, gnbList, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	for _, gnb := range gnbList.Items {
		v := r.auditGNodeB(&gnb, policy)
		violations = append(violations, v...)
	}

	// Reconcile the requested enforcement controls, collecting failures so status
	// reflects whether they were ACTUALLY applied — not just that we tried.
	var enforceErrs []string
	if policy.Spec.NetworkIsolation {
		op, err := r.reconcileNetworkPolicies(ctx, policy)
		if err != nil {
			logger.Error(err, "Failed to reconcile network policies")
			enforceErrs = append(enforceErrs, "network isolation: "+err.Error())
		} else {
			// Surface what happened so a drift correction is visible, not silent —
			// someone edited the NetworkPolicy for a reason and should see it was reverted.
			reason, npMsg := "InSync", "NetworkPolicy matches the required isolation spec"
			switch op {
			case controllerutil.OperationResultCreated:
				reason, npMsg = "Created", "Created the isolation NetworkPolicy"
			case controllerutil.OperationResultUpdated:
				reason, npMsg = "DriftCorrected", "Reverted out-of-band changes to the isolation NetworkPolicy"
				logger.Info("corrected NetworkPolicy drift", "policy", policy.Name)
			}
			setCondition(&policy.Status.Conditions, metav1.Condition{
				Type:               "NetworkPolicyReconciled",
				Status:             metav1.ConditionTrue,
				Reason:             reason,
				Message:            npMsg,
				LastTransitionTime: metav1.Now(),
			})
		}
	}

	// Reconcile Falco rules ConfigMap
	if policy.Spec.RuntimeMonitoring {
		if err := r.reconcileFalcoRules(ctx, policy); err != nil {
			logger.Error(err, "Failed to reconcile Falco rules")
			enforceErrs = append(enforceErrs, "runtime monitoring: "+err.Error())
		}
	}

	// Controls the operator asked for that this controller has no implementation
	// for. A permanent gap, not a transient failure — see unenforceableControls.
	unenforceable := unenforceableControls(policy.Spec)

	// Update status. Compliance can only be asserted over controls that were
	// actually evaluated: with an unenforceable control requested, the audit has no
	// basis to claim the policy is satisfied, so it must not claim it.
	policy.Status.Compliant = len(violations) == 0 && len(unenforceable) == 0
	policy.Status.Violations = violations
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Audited",
		Status:             metav1.ConditionTrue,
		Reason:             "AuditComplete",
		Message:            fmt.Sprintf("Audited %d GNodeBs, found %d violations", len(gnbList.Items), len(violations)),
		LastTransitionTime: metav1.Now(),
	})

	// Enforced reflects whether the requested controls were actually applied.
	// A security policy that reports compliant/audited while its NetworkPolicy or
	// Falco rules failed to apply is the dangerous case — surface it.
	// A control this controller cannot enforce at all is the same lie told more
	// quietly, so it lands in the same condition rather than passing as applied.
	problems := append([]string(nil), enforceErrs...)
	if len(unenforceable) > 0 {
		problems = append(problems, "no implementation for: "+strings.Join(unenforceable, ", "))
	}
	enforced := len(problems) == 0
	enforcedStatus := metav1.ConditionTrue
	reason, msg := "ControlsApplied", "Requested security controls applied"
	if !enforced {
		enforcedStatus = metav1.ConditionFalse
		// An apply failure is the more urgent of the two and is retryable, so it
		// names the reason when both are present.
		reason = "ControlsUnimplemented"
		if len(enforceErrs) > 0 {
			reason = "EnforcementFailed"
		}
		msg = "Enforcement incomplete: " + strings.Join(problems, "; ")
	}
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Enforced",
		Status:             enforcedStatus,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, policy); err != nil {
		return ctrl.Result{}, err
	}

	// A control that failed to APPLY is retryable: fail closed and requeue with
	// backoff rather than sitting green for 60s. A control with no implementation is
	// not retryable — no amount of backoff produces one — so it is reported in
	// status and left to the normal re-audit cadence instead of hot-looping.
	if len(enforceErrs) > 0 {
		return ctrl.Result{}, fmt.Errorf("enforcement incomplete: %s", strings.Join(enforceErrs, "; "))
	}

	// Re-audit every 60 seconds
	return ctrl.Result{RequeueAfter: 60e9}, nil
}

func (r *RANSecurityPolicyReconciler) auditGNodeB(gnb *ranv1alpha1.GNodeB, policy *ranv1alpha1.RANSecurityPolicy) []ranv1alpha1.SecurityViolation {
	var violations []ranv1alpha1.SecurityViolation

	// Check image against allowed registries
	if len(policy.Spec.AllowedRegistries) > 0 {
		allowed := false
		for _, reg := range policy.Spec.AllowedRegistries {
			if strings.HasPrefix(gnb.Spec.Image, reg) {
				allowed = true
				break
			}
		}
		if !allowed {
			violations = append(violations, ranv1alpha1.SecurityViolation{
				GNodeBName: gnb.Name,
				Rule:       "AllowedRegistries",
				Severity:   "Critical",
				Message:    fmt.Sprintf("Image %q not from allowed registry", gnb.Spec.Image),
			})
		}
	}

	// Check if security policy is referenced
	if gnb.Spec.SecurityPolicyRef == "" {
		violations = append(violations, ranv1alpha1.SecurityViolation{
			GNodeBName: gnb.Name,
			Rule:       "SecurityPolicyRef",
			Severity:   "Warning",
			Message:    "GNodeB has no security policy reference",
		})
	}

	return violations
}

// unenforceableControls returns the controls a policy requests that this
// controller has no implementation for. Requesting one is not an operator error,
// it is a gap here — but it must never be reported as applied: an operator who
// sets seccompProfile, reads back Enforced=True/ControlsApplied and concludes a
// seccomp profile is in force has been told something untrue by this code.
//
// Unlike a failure to apply, a missing implementation is permanent, so callers
// report it WITHOUT returning an error; requeueing with backoff cannot close a
// gap that only new code closes.
//
// Every field of RANSecurityPolicySpec must be either implemented by this
// controller or listed here. TestRANSecurityPolicySpec_EveryFieldIsClassified
// fails if a new field is neither.
func unenforceableControls(spec ranv1alpha1.RANSecurityPolicySpec) []string {
	var out []string
	if spec.ImageScanningEnabled {
		out = append(out, "imageScanningEnabled")
	}
	if spec.MaxCVSSScore > 0 {
		out = append(out, "maxCVSSScore")
	}
	if spec.SeccompProfile != "" {
		out = append(out, "seccompProfile")
	}
	if spec.EncryptFronthaul {
		out = append(out, "encryptFronthaul")
	}
	return out
}

// desiredFronthaulPolicySpec is the single source of truth for the isolation
// NetworkPolicy. Both create and drift-correction reconcile against it, so an NP
// that was edited to be more permissive is restored, not just recreated when
// deleted.
func desiredFronthaulPolicySpec() networkingv1.NetworkPolicySpec {
	fronthaulPort := intstr.FromInt(44000) // eCPRI default port
	proto := corev1.ProtocolUDP
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{
			MatchLabels: map[string]string{"app.kubernetes.io/name": "gnodeb"},
		},
		PolicyTypes: []networkingv1.PolicyType{
			networkingv1.PolicyTypeIngress,
			networkingv1.PolicyTypeEgress,
		},
		Ingress: []networkingv1.NetworkPolicyIngressRule{
			{Ports: []networkingv1.NetworkPolicyPort{{Port: &fronthaulPort, Protocol: &proto}}},                                                               // fronthaul eCPRI
			{Ports: []networkingv1.NetworkPolicyPort{{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 9090}, Protocol: protocolPtr(corev1.ProtocolTCP)}}}, // metrics
		},
		Egress: []networkingv1.NetworkPolicyEgressRule{
			{Ports: []networkingv1.NetworkPolicyPort{{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 53}, Protocol: protocolPtr(corev1.ProtocolUDP)}}}, // DNS
			{Ports: []networkingv1.NetworkPolicyPort{{Port: &fronthaulPort, Protocol: &proto}}},                                                             // fronthaul eCPRI egress
		},
	}
}

// reconcileNetworkPolicies enforces the isolation NetworkPolicy by reconciling
// its SPEC, not merely its existence. It returns the operation performed so the
// caller can surface a drift correction (an NP widened out-of-band is reverted).
func (r *RANSecurityPolicyReconciler) reconcileNetworkPolicies(ctx context.Context, policy *ranv1alpha1.RANSecurityPolicy) (controllerutil.OperationResult, error) {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policy.Name + "-fronthaul-isolation",
			Namespace: policy.Namespace,
		},
	}
	return controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		if np.Labels == nil {
			np.Labels = map[string]string{}
		}
		np.Labels["app.kubernetes.io/managed-by"] = "wavekube"
		np.Labels["ran.wavekube.io/policy"] = policy.Name
		// Restore the required spec — this is what reverts any out-of-band widening.
		np.Spec = desiredFronthaulPolicySpec()
		return ctrl.SetControllerReference(policy, np, r.Scheme)
	})
}

func (r *RANSecurityPolicyReconciler) reconcileFalcoRules(ctx context.Context, policy *ranv1alpha1.RANSecurityPolicy) error {
	cm := &corev1.ConfigMap{}
	cmName := types.NamespacedName{Name: policy.Name + "-falco-rules", Namespace: policy.Namespace}

	if err := r.Get(ctx, cmName, cm); errors.IsNotFound(err) {
		falcoRules := `- rule: Unauthorized GPU Memory Access in RAN
  desc: Detect unauthorized access to GPU device files from non-RAN containers
  condition: >
    open_write and container and
    fd.name startswith /dev/nvidia and
    not k8s.pod.label.app.kubernetes.io/managed-by = "wavekube"
  output: >
    Unauthorized GPU access in non-wavekube container
    (user=%user.name command=%proc.cmdline container=%container.name gpu_device=%fd.name)
  priority: CRITICAL

- rule: RAN Config Tampering
  desc: Detect modification of RAN configuration files at runtime
  condition: >
    open_write and container and
    k8s.pod.label.app.kubernetes.io/name = "gnodeb" and
    (fd.name contains "cuPHY" or fd.name contains "cuMAC" or fd.name contains "aerial")
  output: >
    RAN configuration modified at runtime
    (user=%user.name file=%fd.name container=%container.name)
  priority: WARNING

- rule: Unexpected Network Connection from RAN Pod
  desc: Detect outbound connections from RAN pods to unexpected destinations
  condition: >
    outbound and container and
    k8s.pod.label.app.kubernetes.io/name = "gnodeb" and
    not fd.sport in (44000, 9090, 53)
  output: >
    Unexpected outbound connection from RAN pod
    (command=%proc.cmdline connection=%fd.name container=%container.name)
  priority: WARNING

- rule: Shell Spawned in RAN Container
  desc: Detect interactive shell access to RAN containers
  condition: >
    spawned_process and container and
    k8s.pod.label.app.kubernetes.io/managed-by = "wavekube" and
    proc.name in (bash, sh, zsh, dash)
  output: >
    Shell spawned in wavekube-managed container
    (user=%user.name shell=%proc.name container=%container.name)
  priority: CRITICAL
`

		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      cmName.Name,
				Namespace: cmName.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "wavekube",
					"falco.org/rules":              "true",
				},
			},
			Data: map[string]string{
				"wavekube_rules.yaml": falcoRules,
			},
		}
		if err := ctrl.SetControllerReference(policy, cm, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, cm)
	}

	return nil
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol { return &p }

func (r *RANSecurityPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ranv1alpha1.RANSecurityPolicy{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}
