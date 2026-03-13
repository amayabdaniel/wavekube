package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

// GNodeBReconciler reconciles a GNodeB object.
type GNodeBReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ran.wavekube.io,resources=gnodebs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ran.wavekube.io,resources=gnodebs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ran.wavekube.io,resources=gnodebs/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *GNodeBReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the GNodeB instance
	gnb := &ranv1alpha1.GNodeB{}
	if err := r.Get(ctx, req.NamespacedName, gnb); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("GNodeB resource not found, likely deleted")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling GNodeB", "name", gnb.Name, "band", gnb.Spec.PHYConfig.Band)

	// Update phase to Initializing if Pending
	if gnb.Status.Phase == "" || gnb.Status.Phase == "Pending" {
		gnb.Status.Phase = "Initializing"
		if err := r.Status().Update(ctx, gnb); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reconcile the Deployment for this GNodeB
	if err := r.reconcileDeployment(ctx, gnb); err != nil {
		gnb.Status.Phase = "Failed"
		_ = r.Status().Update(ctx, gnb)
		return ctrl.Result{}, err
	}

	// Reconcile the metrics Service
	if err := r.reconcileService(ctx, gnb); err != nil {
		return ctrl.Result{}, err
	}

	// Check security policy if referenced
	if gnb.Spec.SecurityPolicyRef != "" {
		if err := r.checkSecurityPolicy(ctx, gnb); err != nil {
			logger.Error(err, "Security policy check failed")
		}
	}

	// Update status
	gnb.Status.Phase = "Running"
	if err := r.Status().Update(ctx, gnb); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *GNodeBReconciler) reconcileDeployment(ctx context.Context, gnb *ranv1alpha1.GNodeB) error {
	deploy := &appsv1.Deployment{}
	deployName := types.NamespacedName{Name: gnb.Name + "-ran", Namespace: gnb.Namespace}

	err := r.Get(ctx, deployName, deploy)
	if errors.IsNotFound(err) {
		deploy = r.buildDeployment(gnb)
		if err := ctrl.SetControllerReference(gnb, deploy, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, deploy)
	}
	if err != nil {
		return err
	}

	// Update existing deployment if spec changed
	deploy.Spec.Replicas = &gnb.Spec.Replicas
	deploy.Spec.Template.Spec.Containers[0].Image = gnb.Spec.Image
	return r.Update(ctx, deploy)
}

func (r *GNodeBReconciler) buildDeployment(gnb *ranv1alpha1.GNodeB) *appsv1.Deployment {
	labels := map[string]string{
		"app.kubernetes.io/name":       "gnodeb",
		"app.kubernetes.io/instance":   gnb.Name,
		"app.kubernetes.io/managed-by": "wavekube",
		"ran.wavekube.io/band":         gnb.Spec.PHYConfig.Band,
	}

	replicas := gnb.Spec.Replicas
	gpuQty := resource.MustParse(fmt.Sprintf("%d", gnb.Spec.GPUResources.Count))

	container := corev1.Container{
		Name:  "aerial-ran",
		Image: gnb.Spec.Image,
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": gpuQty,
			},
			Requests: corev1.ResourceList{
				"nvidia.com/gpu": gpuQty,
			},
		},
		Env: []corev1.EnvVar{
			{Name: "AERIAL_PHY_BANDWIDTH", Value: fmt.Sprintf("%d", gnb.Spec.PHYConfig.Bandwidth)},
			{Name: "AERIAL_PHY_NUMEROLOGY", Value: fmt.Sprintf("%d", gnb.Spec.PHYConfig.Numerology)},
			{Name: "AERIAL_PHY_BAND", Value: gnb.Spec.PHYConfig.Band},
			{Name: "AERIAL_PHY_MAX_UES", Value: fmt.Sprintf("%d", gnb.Spec.PHYConfig.MaxUEs)},
			{Name: "AERIAL_FRONTHAUL_IFACE", Value: gnb.Spec.Network.FronthaulInterface},
		},
		SecurityContext: &corev1.SecurityContext{
			Privileged: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"NET_ADMIN", "IPC_LOCK", "SYS_NICE"},
				Drop: []corev1.Capability{"ALL"},
			},
		},
	}

	// Add RDMA host network if enabled
	hostNetwork := gnb.Spec.GPUResources.EnableRDMA

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gnb.Name + "-ran",
			Namespace: gnb.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers:  []corev1.Container{container},
					HostNetwork: hostNetwork,
					Tolerations: []corev1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					NodeSelector: r.buildNodeSelector(gnb),
				},
			},
		},
	}

	return deploy
}

func (r *GNodeBReconciler) buildNodeSelector(gnb *ranv1alpha1.GNodeB) map[string]string {
	selector := map[string]string{
		"nvidia.com/gpu.present": "true",
	}
	if gnb.Spec.GPUResources.Type != "" {
		selector["nvidia.com/gpu.product"] = gnb.Spec.GPUResources.Type
	}
	return selector
}

func (r *GNodeBReconciler) reconcileService(ctx context.Context, gnb *ranv1alpha1.GNodeB) error {
	svc := &corev1.Service{}
	svcName := types.NamespacedName{Name: gnb.Name + "-metrics", Namespace: gnb.Namespace}

	if err := r.Get(ctx, svcName, svc); errors.IsNotFound(err) {
		svc = &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      gnb.Name + "-metrics",
				Namespace: gnb.Namespace,
				Labels: map[string]string{
					"app.kubernetes.io/name":       "gnodeb",
					"app.kubernetes.io/instance":   gnb.Name,
					"app.kubernetes.io/managed-by": "wavekube",
				},
				Annotations: map[string]string{
					"prometheus.io/scrape": "true",
					"prometheus.io/port":   "9090",
				},
			},
			Spec: corev1.ServiceSpec{
				Selector: map[string]string{
					"app.kubernetes.io/instance": gnb.Name,
				},
				Ports: []corev1.ServicePort{
					{Name: "metrics", Port: 9090, Protocol: corev1.ProtocolTCP},
				},
			},
		}
		if err := ctrl.SetControllerReference(gnb, svc, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, svc)
	}
	return nil
}

func (r *GNodeBReconciler) checkSecurityPolicy(ctx context.Context, gnb *ranv1alpha1.GNodeB) error {
	policy := &ranv1alpha1.RANSecurityPolicy{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      gnb.Spec.SecurityPolicyRef,
		Namespace: gnb.Namespace,
	}, policy); err != nil {
		return err
	}

	// Validate image against allowed registries
	if len(policy.Spec.AllowedRegistries) > 0 {
		allowed := false
		for _, reg := range policy.Spec.AllowedRegistries {
			if len(gnb.Spec.Image) >= len(reg) && gnb.Spec.Image[:len(reg)] == reg {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("image %s not from allowed registry", gnb.Spec.Image)
		}
	}

	return nil
}

func boolPtr(b bool) *bool { return &b }

func (r *GNodeBReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ranv1alpha1.GNodeB{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
