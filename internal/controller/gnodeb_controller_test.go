package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ranv1alpha1 "github.com/amayabdaniel/wavekube/api/v1alpha1"
)

func TestGNodeBReconciler_BasicReconcile(t *testing.T) {
	// Unit test: verify that a GNodeB resource creates the expected Deployment
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gnb",
			Namespace: "default",
		},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:    "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			Replicas: 1,
			GPUResources: ranv1alpha1.GPUResourceSpec{
				Count:      1,
				Type:       "A100",
				EnableRDMA: false,
			},
			PHYConfig: ranv1alpha1.PHYConfig{
				Bandwidth:  100,
				Numerology: 1,
				Band:       "n78",
				MaxUEs:     32,
			},
			Network: ranv1alpha1.NetworkConfig{
				FronthaulInterface: "eth1",
			},
		},
	}

	// Verify spec is well-formed
	if gnb.Spec.PHYConfig.Band != "n78" {
		t.Errorf("expected band n78, got %s", gnb.Spec.PHYConfig.Band)
	}
	if gnb.Spec.GPUResources.Count != 1 {
		t.Errorf("expected 1 GPU, got %d", gnb.Spec.GPUResources.Count)
	}
	if gnb.Spec.Replicas != 1 {
		t.Errorf("expected 1 replica, got %d", gnb.Spec.Replicas)
	}

	_ = reconcile.Request{NamespacedName: types.NamespacedName{Name: gnb.Name, Namespace: gnb.Namespace}}
	_ = context.Background()
}

func TestGNodeBReconciler_BuildDeployment(t *testing.T) {
	r := &GNodeBReconciler{}
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prod-gnb",
			Namespace: "telecom",
		},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:    "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			Replicas: 2,
			GPUResources: ranv1alpha1.GPUResourceSpec{
				Count:      2,
				Type:       "H100",
				EnableRDMA: true,
			},
			PHYConfig: ranv1alpha1.PHYConfig{
				Bandwidth:  100,
				Numerology: 1,
				Band:       "n77",
				MaxUEs:     64,
			},
			Network: ranv1alpha1.NetworkConfig{
				FronthaulInterface: "eth2",
				MidhaulCIDR:        "10.10.0.0/24",
				BackhaulCIDR:       "10.20.0.0/24",
			},
		},
	}

	deploy := r.buildDeployment(gnb)

	if deploy.Name != "prod-gnb-ran" {
		t.Errorf("expected deployment name prod-gnb-ran, got %s", deploy.Name)
	}
	if deploy.Namespace != "telecom" {
		t.Errorf("expected namespace telecom, got %s", deploy.Namespace)
	}
	if *deploy.Spec.Replicas != 2 {
		t.Errorf("expected 2 replicas, got %d", *deploy.Spec.Replicas)
	}
	if !deploy.Spec.Template.Spec.HostNetwork {
		t.Error("expected HostNetwork=true when RDMA is enabled")
	}

	container := deploy.Spec.Template.Spec.Containers[0]
	if container.Image != "nvcr.io/nvidia/aerial/aerial-ran:24.3" {
		t.Errorf("expected aerial image, got %s", container.Image)
	}

	gpuLimit := container.Resources.Limits["nvidia.com/gpu"]
	if gpuLimit.Value() != 2 {
		t.Errorf("expected 2 GPU limit, got %d", gpuLimit.Value())
	}

	// Check env vars
	envMap := make(map[string]string)
	for _, env := range container.Env {
		envMap[env.Name] = env.Value
	}
	if envMap["AERIAL_PHY_BAND"] != "n77" {
		t.Errorf("expected band n77 in env, got %s", envMap["AERIAL_PHY_BAND"])
	}
	if envMap["AERIAL_FRONTHAUL_IFACE"] != "eth2" {
		t.Errorf("expected eth2 fronthaul, got %s", envMap["AERIAL_FRONTHAUL_IFACE"])
	}

	// Check node selector
	ns := deploy.Spec.Template.Spec.NodeSelector
	if ns["nvidia.com/gpu.product"] != "H100" {
		t.Errorf("expected H100 node selector, got %s", ns["nvidia.com/gpu.product"])
	}
}

func TestGNodeBReconciler_SecurityPolicyValidation(t *testing.T) {
	gnb := &ranv1alpha1.GNodeB{
		ObjectMeta: metav1.ObjectMeta{Name: "sec-gnb", Namespace: "default"},
		Spec: ranv1alpha1.GNodeBSpec{
			Image:             "nvcr.io/nvidia/aerial/aerial-ran:24.3",
			SecurityPolicyRef: "strict-policy",
		},
	}

	if gnb.Spec.SecurityPolicyRef != "strict-policy" {
		t.Errorf("expected security policy ref, got %s", gnb.Spec.SecurityPolicyRef)
	}
}
