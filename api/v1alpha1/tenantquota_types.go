/*
Copyright 2026 The llm-deployment-operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SliceLimit caps the number of MIG instances of one profile.
type SliceLimit struct {
	// Profile is the MIG profile, e.g. 1g.20gb.
	// +kubebuilder:validation:Pattern=`^[1-7]g\.[0-9]+gb$`
	Profile string `json:"profile"`
	// Max is the maximum number of instances.
	// +kubebuilder:validation:Minimum=0
	Max int32 `json:"max"`
}

// SliceCount is the number of MIG instances of one profile in use.
type SliceCount struct {
	Profile string `json:"profile"`
	Count   int32  `json:"count"`
}

// TenantQuotaSpec defines the limits for one tenant.
type TenantQuotaSpec struct {
	// Tenant is matched against ModelDeployment spec.tenant.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Tenant string `json:"tenant"`

	// MIGSlices caps MIG instances per profile across all of the tenant's
	// deployments, including canaries. Profiles that are not listed are only
	// bound by maxGPUMemoryGiB.
	// +listType=map
	// +listMapKey=profile
	// +optional
	MIGSlices []SliceLimit `json:"migSlices,omitempty"`

	// MaxGPUMemoryGiB caps the nominal memory of all MIG instances the tenant
	// holds (20 GiB per 1g.20gb instance, 80 GiB per 7g.80gb instance).
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxGPUMemoryGiB *int64 `json:"maxGPUMemoryGiB,omitempty"`
}

// TenantQuotaStatus reports the tenant's current usage.
type TenantQuotaStatus struct {
	// ObservedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// UsedSlices are the MIG instances held by the tenant's deployments.
	// +listType=map
	// +listMapKey=profile
	// +optional
	UsedSlices []SliceCount `json:"usedSlices,omitempty"`

	// UsedGPUMemoryGiB is the nominal memory of those instances.
	// +optional
	UsedGPUMemoryGiB int64 `json:"usedGPUMemoryGiB,omitempty"`

	// Deployments is the number of ModelDeployments of the tenant.
	// +optional
	Deployments int32 `json:"deployments,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=tq
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Used GiB",type=integer,JSONPath=`.status.usedGPUMemoryGiB`
// +kubebuilder:printcolumn:name="Max GiB",type=integer,JSONPath=`.spec.maxGPUMemoryGiB`
// +kubebuilder:printcolumn:name="Deployments",type=integer,JSONPath=`.status.deployments`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// TenantQuota limits the MIG slices a tenant's ModelDeployments may claim.
type TenantQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TenantQuotaSpec   `json:"spec"`
	Status TenantQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TenantQuotaList contains a list of TenantQuota.
type TenantQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TenantQuota `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TenantQuota{}, &TenantQuotaList{})
}
