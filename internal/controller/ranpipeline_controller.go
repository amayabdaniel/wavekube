package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
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

type RANPipelineReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ran.wavekube.io,resources=ranpipelines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ran.wavekube.io,resources=ranpipelines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

func (r *RANPipelineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pipeline := &ranv1alpha1.RANPipeline{}
	if err := r.Get(ctx, req.NamespacedName, pipeline); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling RANPipeline", "name", pipeline.Name, "gnodeb", pipeline.Spec.GNodeBRef)

	// Verify referenced GNodeB exists and is Running
	gnb := &ranv1alpha1.GNodeB{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      pipeline.Spec.GNodeBRef,
		Namespace: pipeline.Namespace,
	}, gnb); err != nil {
		if errors.IsNotFound(err) {
			pipeline.Status.Phase = "Failed"
			meta := metav1.Condition{
				Type:               "GNodeBReady",
				Status:             metav1.ConditionFalse,
				Reason:             "GNodeBNotFound",
				Message:            fmt.Sprintf("Referenced GNodeB %q not found", pipeline.Spec.GNodeBRef),
				LastTransitionTime: metav1.Now(),
			}
			setCondition(&pipeline.Status.Conditions, meta)
			_ = r.Status().Update(ctx, pipeline)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if gnb.Status.Phase != "Running" {
		pipeline.Status.Phase = "Pending"
		meta := metav1.Condition{
			Type:               "GNodeBReady",
			Status:             metav1.ConditionFalse,
			Reason:             "GNodeBNotReady",
			Message:            fmt.Sprintf("GNodeB %q is in phase %q, waiting for Running", gnb.Name, gnb.Status.Phase),
			LastTransitionTime: metav1.Now(),
		}
		setCondition(&pipeline.Status.Conditions, meta)
		_ = r.Status().Update(ctx, pipeline)
		return ctrl.Result{RequeueAfter: 10e9}, nil // requeue in 10s
	}

	// Reconcile the pipeline Job
	if err := r.reconcileJob(ctx, pipeline, gnb); err != nil {
		pipeline.Status.Phase = "Failed"
		_ = r.Status().Update(ctx, pipeline)
		return ctrl.Result{}, err
	}

	pipeline.Status.Phase = "Running"
	setCondition(&pipeline.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "PipelineDeployed",
		Message:            "Pipeline job created and running",
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, pipeline); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RANPipelineReconciler) reconcileJob(ctx context.Context, pipeline *ranv1alpha1.RANPipeline, gnb *ranv1alpha1.GNodeB) error {
	job := &batchv1.Job{}
	jobName := types.NamespacedName{Name: pipeline.Name + "-pipeline", Namespace: pipeline.Namespace}

	if err := r.Get(ctx, jobName, job); errors.IsNotFound(err) {
		job = r.buildJob(pipeline, gnb)
		if err := ctrl.SetControllerReference(pipeline, job, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, job)
	}
	return nil
}

func (r *RANPipelineReconciler) buildJob(pipeline *ranv1alpha1.RANPipeline, gnb *ranv1alpha1.GNodeB) *batchv1.Job {
	labels := map[string]string{
		"app.kubernetes.io/name":       "ran-pipeline",
		"app.kubernetes.io/instance":   pipeline.Name,
		"app.kubernetes.io/managed-by": "wavekube",
		"ran.wavekube.io/gnodeb":       pipeline.Spec.GNodeBRef,
	}

	gpuQty := resource.MustParse(fmt.Sprintf("%d", pipeline.Spec.Resources.GPUCount))
	memQty := resource.MustParse(fmt.Sprintf("%dMi", pipeline.Spec.Resources.MemoryMi))
	cpuQty := resource.MustParse(fmt.Sprintf("%d", pipeline.Spec.Resources.CPUCores))

	backoffLimit := int32(3)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pipeline.Name + "-pipeline",
			Namespace: pipeline.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{
							Name:  "aerial-pipeline",
							Image: pipeline.Spec.Image,
							Args:  []string{"--pipeline", pipeline.Spec.PipelineDefinition},
							Env: []corev1.EnvVar{
								{Name: "GNODEB_NAME", Value: gnb.Name},
								{Name: "AERIAL_PHY_BAND", Value: gnb.Spec.PHYConfig.Band},
								{Name: "AERIAL_PHY_BANDWIDTH", Value: fmt.Sprintf("%d", gnb.Spec.PHYConfig.Bandwidth)},
								{Name: "AERIAL_PHY_NUMEROLOGY", Value: fmt.Sprintf("%d", gnb.Spec.PHYConfig.Numerology)},
							},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu":  gpuQty,
									corev1.ResourceMemory: memQty,
									corev1.ResourceCPU:    cpuQty,
								},
								Requests: corev1.ResourceList{
									"nvidia.com/gpu":  gpuQty,
									corev1.ResourceMemory: memQty,
									corev1.ResourceCPU:    cpuQty,
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: boolPtr(false),
								Capabilities: &corev1.Capabilities{
									Drop: []corev1.Capability{"ALL"},
									Add:  []corev1.Capability{"IPC_LOCK"},
								},
							},
						},
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
				},
			},
		},
	}
}

func setCondition(conditions *[]metav1.Condition, condition metav1.Condition) {
	for i, c := range *conditions {
		if c.Type == condition.Type {
			(*conditions)[i] = condition
			return
		}
	}
	*conditions = append(*conditions, condition)
}

func (r *RANPipelineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ranv1alpha1.RANPipeline{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
