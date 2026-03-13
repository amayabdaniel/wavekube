// Package v1alpha1 contains API Schema definitions for wavekube v1alpha1 API group.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion = schema.GroupVersion{Group: "ran.wavekube.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&GNodeB{}, &GNodeBList{})
	SchemeBuilder.Register(&RANPipeline{}, &RANPipelineList{})
	SchemeBuilder.Register(&RANSecurityPolicy{}, &RANSecurityPolicyList{})
}
