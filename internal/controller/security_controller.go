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

	// Reconcile network isolation
	if policy.Spec.NetworkIsolation {
		if err := r.reconcileNetworkPolicies(ctx, policy); err != nil {
			logger.Error(err, "Failed to reconcile network policies")
		}
	}

	// Reconcile Falco rules ConfigMap
	if policy.Spec.RuntimeMonitoring {
		if err := r.reconcileFalcoRules(ctx, policy); err != nil {
			logger.Error(err, "Failed to reconcile Falco rules")
		}
	}

	// Update status
	policy.Status.Compliant = len(violations) == 0
	policy.Status.Violations = violations
	setCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               "Audited",
		Status:             metav1.ConditionTrue,
		Reason:             "AuditComplete",
		Message:            fmt.Sprintf("Audited %d GNodeBs, found %d violations", len(gnbList.Items), len(violations)),
		LastTransitionTime: metav1.Now(),
	})

	if err := r.Status().Update(ctx, policy); err != nil {
		return ctrl.Result{}, err
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

func (r *RANSecurityPolicyReconciler) reconcileNetworkPolicies(ctx context.Context, policy *ranv1alpha1.RANSecurityPolicy) error {
	// Fronthaul isolation — only allow traffic on fronthaul ports between RAN pods
	fronthaulPolicy := &networkingv1.NetworkPolicy{}
	npName := types.NamespacedName{Name: policy.Name + "-fronthaul-isolation", Namespace: policy.Namespace}

	if err := r.Get(ctx, npName, fronthaulPolicy); errors.IsNotFound(err) {
		fronthaulPort := intstr.FromInt(44000) // eCPRI default port
		proto := corev1.ProtocolUDP

		fronthaulPolicy = &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      npName.Name,
				Namespace: npName.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "wavekube",
					"ran.wavekube.io/policy":       policy.Name,
				},
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app.kubernetes.io/name": "gnodeb",
					},
				},
				PolicyTypes: []networkingv1.PolicyType{
					networkingv1.PolicyTypeIngress,
					networkingv1.PolicyTypeEgress,
				},
				Ingress: []networkingv1.NetworkPolicyIngressRule{
					{
						// Allow fronthaul eCPRI
						Ports: []networkingv1.NetworkPolicyPort{
							{Port: &fronthaulPort, Protocol: &proto},
						},
					},
					{
						// Allow metrics scraping
						Ports: []networkingv1.NetworkPolicyPort{
							{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 9090}, Protocol: protocolPtr(corev1.ProtocolTCP)},
						},
					},
				},
				Egress: []networkingv1.NetworkPolicyEgressRule{
					{
						// Allow DNS
						Ports: []networkingv1.NetworkPolicyPort{
							{Port: &intstr.IntOrString{Type: intstr.Int, IntVal: 53}, Protocol: protocolPtr(corev1.ProtocolUDP)},
						},
					},
					{
						// Allow fronthaul eCPRI egress
						Ports: []networkingv1.NetworkPolicyPort{
							{Port: &fronthaulPort, Protocol: &proto},
						},
					},
				},
			},
		}
		if err := ctrl.SetControllerReference(policy, fronthaulPolicy, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, fronthaulPolicy)
	}

	return nil
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
