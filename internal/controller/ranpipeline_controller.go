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
			// Return the status-update error so a failed write requeues, rather
			// than silently leaving the pipeline's reported phase diverged from
			// reality (this path does not otherwise requeue).
			return ctrl.Result{}, r.Status().Update(ctx, pipeline)
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
	job, err := r.reconcileJob(ctx, pipeline, gnb)
	if err != nil {
		pipeline.Status.Phase = "Failed"
		_ = r.Status().Update(ctx, pipeline)
		return ctrl.Result{}, err
	}

	// Reflect the Job's ACTUAL state — do not report Running/Ready just because
	// the Job object exists. A failed or not-yet-started Job must not surface as
	// a running pipeline.
	phase, ready, reason, msg := pipelinePhaseFromJob(job)
	pipeline.Status.Phase = phase
	readyStatus := metav1.ConditionFalse
	if ready {
		readyStatus = metav1.ConditionTrue
	}
	setCondition(&pipeline.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             readyStatus,
		Reason:             reason,
		Message:            msg,
		LastTransitionTime: metav1.Now(),
	})
	if err := r.Status().Update(ctx, pipeline); err != nil {
		return ctrl.Result{}, err
	}

	// Requeue only while the pipeline is still progressing toward a terminal
	// state, so status keeps tracking a Job that hasn't reported Active yet.
	// Succeeded and Failed are terminal: a finished pipeline is not requeued
	// here — re-reconciliation on any further Job change is driven by the
	// Owns(&batchv1.Job{}) watch, not a timer, so a completed pipeline does not
	// spin on a 10s loop forever.
	if phase == "Pending" {
		return ctrl.Result{RequeueAfter: 10e9}, nil
	}
	return ctrl.Result{}, nil
}

// pipelinePhaseFromJob maps a batch Job's observed state to the pipeline phase,
// Ready flag, condition reason and message.
func pipelinePhaseFromJob(job *batchv1.Job) (phase string, ready bool, reason, msg string) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return "Failed", false, "PipelineJobFailed", "Pipeline job failed: " + c.Reason
		}
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			// Terminal success. The pipeline is modelled as a run-to-completion
			// batch Job (BackoffLimit/OnFailure), not a long-lived Deployment, so a
			// completed Job means the pipeline is DONE, not "Running" — reporting
			// Running here is the same plausible-wrong-state this mapping exists to
			// prevent. Ready=false by design: Ready marks a pipeline that is up and
			// serving now (see the Active branch); a finished Job is not serving.
			// The successful outcome is carried by phase=Succeeded, not by Ready.
			return "Succeeded", false, "PipelineJobComplete", "Pipeline job completed successfully"
		}
	}
	if job.Status.Active > 0 {
		return "Running", true, "PipelineJobActive", "Pipeline job is active"
	}
	return "Pending", false, "PipelineJobPending", "Pipeline job created, waiting to start"
}

// reconcileJob ensures the pipeline Job exists and returns its current state so
// the caller can reflect it in the pipeline status.
func (r *RANPipelineReconciler) reconcileJob(ctx context.Context, pipeline *ranv1alpha1.RANPipeline, gnb *ranv1alpha1.GNodeB) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	jobName := types.NamespacedName{Name: pipeline.Name + "-pipeline", Namespace: pipeline.Namespace}

	err := r.Get(ctx, jobName, job)
	if errors.IsNotFound(err) {
		job = r.buildJob(pipeline, gnb)
		if err := ctrl.SetControllerReference(pipeline, job, r.Scheme); err != nil {
			return nil, err
		}
		if err := r.Create(ctx, job); err != nil {
			return nil, err
		}
		return job, nil
	}
	if err != nil {
		return nil, err
	}
	return job, nil
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
									"nvidia.com/gpu":      gpuQty,
									corev1.ResourceMemory: memQty,
									corev1.ResourceCPU:    cpuQty,
								},
								Requests: corev1.ResourceList{
									"nvidia.com/gpu":      gpuQty,
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
