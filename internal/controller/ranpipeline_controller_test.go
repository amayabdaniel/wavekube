package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

func TestRANPipelineReconciler_BuildJob(t *testing.T) {
	r := &RANPipelineReconciler{}

	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "test-gnb", Namespace: "default"},
		Spec: ranv1alpha1.GNodeBSpec{
			PHYConfig: ranv1alpha1.PHYConfig{
				Band:       "n78",
				Bandwidth:  100,
				Numerology: 1,
			},
		},
	}

	pipeline := &ranv1alpha1.RANPipeline{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pipeline", Namespace: "default"},
		Spec: ranv1alpha1.RANPipelineSpec{
			Image:              "nvcr.io/nvidia/aerial/aerial-framework:24.3",
			PipelineDefinition: "dl_pipeline_100mhz",
			GNodeBRef:          "test-gnb",
			Resources: ranv1alpha1.PipelineResources{
				GPUCount: 1,
				MemoryMi: 4096,
				CPUCores: 4,
				GPUType:  "A100",
			},
		},
	}

	job := r.buildJob(pipeline, gnb)

	if job.Name != "test-pipeline-pipeline" {
		t.Errorf("expected job name test-pipeline-pipeline, got %s", job.Name)
	}

	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "nvcr.io/nvidia/aerial/aerial-framework:24.3" {
		t.Errorf("expected aerial framework image, got %s", container.Image)
	}

	gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
	if gpuLimit.Value() != 1 {
		t.Errorf("expected 1 GPU, got %d", gpuLimit.Value())
	}

	// Verify env vars pass through GNodeB PHY config
	envMap := make(map[string]string)
	for _, env := range container.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["GNODEB_NAME"] != "test-gnb" {
		t.Errorf("expected GNODEB_NAME=test-gnb, got %s", envMap["GNODEB_NAME"])
	}
	if envMap["AERIAL_PHY_BAND"] != "n78" {
		t.Errorf("expected AERIAL_PHY_BAND=n78, got %s", envMap["AERIAL_PHY_BAND"])
	}

	// Verify backoff limit
	if *job.Spec.BackoffLimit != 3 {
		t.Errorf("expected backoff limit 3, got %d", *job.Spec.BackoffLimit)
	}

	// Verify args
	if len(container.Args) != 2 || container.Args[1] != "dl_pipeline_100mhz" {
		t.Errorf("expected pipeline definition in args, got %v", container.Args)
	}
}

func TestRANPipelineReconciler_Labels(t *testing.T) {
	r := &RANPipelineReconciler{}

	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "gnb-01", Namespace: "telecom"},
		Spec:       ranv1alpha1.GNodeBSpec{PHYConfig: ranv1alpha1.PHYConfig{Band: "n77"}},
	}

	pipeline := &ranv1alpha1.RANPipeline{
		ObjectMeta: metav1.ObjectMeta{Name: "pipe-01", Namespace: "telecom"},
		Spec: ranv1alpha1.RANPipelineSpec{
			Image:     "nvcr.io/nvidia/aerial/aerial-framework:24.3",
			GNodeBRef: "gnb-01",
			Resources: ranv1alpha1.PipelineResources{GPUCount: 1, MemoryMi: 2048, CPUCores: 2},
		},
	}

	job := r.buildJob(pipeline, gnb)

	labels := job.Spec.Template.Labels
	if labels["ran.wavekube.io/gnodeb"] != "gnb-01" {
		t.Errorf("expected gnodeb label, got %s", labels["ran.wavekube.io/gnodeb"])
	}
	if labels["app.kubernetes.io/managed-by"] != "wavekube" {
		t.Errorf("expected managed-by label, got %s", labels["app.kubernetes.io/managed-by"])
	}
}
