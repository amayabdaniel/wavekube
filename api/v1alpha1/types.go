// Package v1alpha1 contains API Schema definitions for wavekube v1alpha1 API group.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- GNodeB ---

// GNodeBSpec defines the desired state of a GPU-accelerated gNodeB.
type GNodeBSpec struct {
	// Image is the Aerial CUDA-Accelerated RAN container image.
	Image string `json:"image"`

	// Replicas is the number of gNodeB instances.
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas,omitempty"`

	// GPUResources specifies GPU requirements per instance.
	GPUResources GPUResourceSpec `json:"gpuResources"`

	// PHYConfig holds cuPHY layer configuration.
	PHYConfig PHYConfig `json:"phyConfig"`

	// Network holds fronthaul/midhaul/backhaul network config.
	Network NetworkConfig `json:"network"`

	// Security holds security policy reference.
	// +optional
	SecurityPolicyRef string `json:"securityPolicyRef,omitempty"`
}

type GPUResourceSpec struct {
	// Count is the number of GPUs required.
	// +kubebuilder:default=1
	Count int32 `json:"count,omitempty"`

	// Type is the GPU model (e.g., "A100", "H100", "L4").
	// +optional
	Type string `json:"type,omitempty"`

	// EnableRDMA enables GPUDirect RDMA for fronthaul.
	// +kubebuilder:default=false
	EnableRDMA bool `json:"enableRDMA,omitempty"`
}

type PHYConfig struct {
	// Bandwidth in MHz (e.g., 20, 40, 100).
	Bandwidth int32 `json:"bandwidth"`

	// Numerology is the 5G NR numerology (0-4).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4
	Numerology int32 `json:"numerology"`

	// Band is the NR frequency band (e.g., "n78", "n77").
	Band string `json:"band"`

	// MaxUEs is the maximum number of concurrent UEs.
	// +kubebuilder:default=32
	MaxUEs int32 `json:"maxUEs,omitempty"`
}

type NetworkConfig struct {
	// FronthaulInterface is the network interface for O-RAN fronthaul (e.g., "eth1").
	FronthaulInterface string `json:"fronthaulInterface"`

	// MidhaulCIDR is the CIDR for midhaul connectivity.
	// +optional
	MidhaulCIDR string `json:"midhaulCIDR,omitempty"`

	// BackhaulCIDR is the CIDR for backhaul connectivity.
	// +optional
	BackhaulCIDR string `json:"backhaulCIDR,omitempty"`
}

type GNodeBStatus struct {
	// Phase is the current lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Initializing;Running;Degraded;Failed
	Phase string `json:"phase,omitempty"`

	// ReadyReplicas is the count of running and healthy instances.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// GPUStatus reports per-GPU health.
	GPUStatus []GPUInstanceStatus `json:"gpuStatus,omitempty"`

	// Conditions represent the latest available observations.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Metrics holds real-time RAN KPIs.
	Metrics RANMetrics `json:"metrics,omitempty"`
}

type GPUInstanceStatus struct {
	DeviceID    string `json:"deviceID"`
	Healthy     bool   `json:"healthy"`
	Temperature int32  `json:"temperature,omitempty"`
	Utilization int32  `json:"utilization,omitempty"`
}

type RANMetrics struct {
	ThroughputDLMbps float64 `json:"throughputDLMbps,omitempty"`
	ThroughputULMbps float64 `json:"throughputULMbps,omitempty"`
	ConnectedUEs     int32   `json:"connectedUEs,omitempty"`
	BLER             float64 `json:"bler,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Band",type=string,JSONPath=`.spec.phyConfig.band`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GNodeB is the Schema for the gnodebs API.
// It represents a GPU-accelerated 5G/6G base station managed by wavekube.
type GNodeB struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GNodeBSpec   `json:"spec,omitempty"`
	Status GNodeBStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GNodeBList contains a list of GNodeB.
type GNodeBList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GNodeB `json:"items"`
}

// --- RANPipeline ---

// RANPipelineSpec defines a GPU-accelerated signal processing pipeline.
type RANPipelineSpec struct {
	// Image is the Aerial Framework pipeline container image.
	Image string `json:"image"`

	// PipelineDefinition is the Python pipeline definition (from aerial-framework).
	PipelineDefinition string `json:"pipelineDefinition"`

	// GNodeBRef references the target GNodeB for this pipeline.
	GNodeBRef string `json:"gnodebRef"`

	// Resources specifies compute requirements.
	Resources PipelineResources `json:"resources"`
}

type PipelineResources struct {
	GPUCount   int32  `json:"gpuCount,omitempty"`
	MemoryMi   int32  `json:"memoryMi,omitempty"`
	CPUCores   int32  `json:"cpuCores,omitempty"`
	GPUType    string `json:"gpuType,omitempty"`
}

type RANPipelineStatus struct {
	Phase      string             `json:"phase,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="GNodeB",type=string,JSONPath=`.spec.gnodebRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RANPipeline is the Schema for GPU signal processing pipelines.
type RANPipeline struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RANPipelineSpec   `json:"spec,omitempty"`
	Status RANPipelineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RANPipelineList contains a list of RANPipeline.
type RANPipelineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RANPipeline `json:"items"`
}

// --- RANSecurityPolicy ---

// RANSecurityPolicySpec defines security controls for RAN workloads.
type RANSecurityPolicySpec struct {
	// ImageScanningEnabled enables container image vulnerability scanning.
	// +kubebuilder:default=true
	ImageScanningEnabled bool `json:"imageScanningEnabled,omitempty"`

	// MaxCVSSScore is the maximum allowed CVSS score for vulnerabilities.
	// +kubebuilder:default=7.0
	MaxCVSSScore float64 `json:"maxCVSSScore,omitempty"`

	// SeccompProfile is the path to the seccomp profile for RAN containers.
	// +optional
	SeccompProfile string `json:"seccompProfile,omitempty"`

	// NetworkIsolation enforces network segmentation between RAN planes.
	// +kubebuilder:default=true
	NetworkIsolation bool `json:"networkIsolation,omitempty"`

	// RuntimeMonitoring enables Falco-based runtime anomaly detection.
	// +kubebuilder:default=true
	RuntimeMonitoring bool `json:"runtimeMonitoring,omitempty"`

	// AllowedRegistries restricts container images to trusted registries.
	AllowedRegistries []string `json:"allowedRegistries,omitempty"`

	// EncryptFronthaul enables eCPRI encryption on the fronthaul interface.
	// +kubebuilder:default=false
	EncryptFronthaul bool `json:"encryptFronthaul,omitempty"`
}

type RANSecurityPolicyStatus struct {
	// Compliant indicates if all referenced GNodeBs meet this policy.
	Compliant  bool               `json:"compliant,omitempty"`
	Violations []SecurityViolation `json:"violations,omitempty"`
	Conditions []metav1.Condition  `json:"conditions,omitempty"`
}

type SecurityViolation struct {
	GNodeBName string `json:"gnodebName"`
	Rule       string `json:"rule"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
	Timestamp  string `json:"timestamp"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Compliant",type=boolean,JSONPath=`.status.compliant`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RANSecurityPolicy defines security controls for RAN workloads.
type RANSecurityPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RANSecurityPolicySpec   `json:"spec,omitempty"`
	Status RANSecurityPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RANSecurityPolicyList contains a list of RANSecurityPolicy.
type RANSecurityPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RANSecurityPolicy `json:"items"`
}
