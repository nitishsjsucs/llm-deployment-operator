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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ModelFamily is the model family.
// +kubebuilder:validation:Enum=llama;qwen
type ModelFamily string

const (
	ModelFamilyLlama ModelFamily = "llama"
	ModelFamilyQwen  ModelFamily = "qwen"
)

// Quantization is the weight format of a model variant.
// +kubebuilder:validation:Enum=fp16;awq;gptq
type Quantization string

const (
	QuantizationFP16 Quantization = "fp16"
	QuantizationAWQ  Quantization = "awq"
	QuantizationGPTQ Quantization = "gptq"
)

// RolloutStrategy selects how a new revision replaces the stable one.
// +kubebuilder:validation:Enum=Canary;Recreate
type RolloutStrategy string

const (
	RolloutStrategyCanary   RolloutStrategy = "Canary"
	RolloutStrategyRecreate RolloutStrategy = "Recreate"
)

// Defaults applied by the API server through the CRD schema and by the
// controller for objects created without them.
const (
	DefaultReplicas               int32 = 1
	DefaultMaxModelLen            int32 = 8192
	DefaultMaxConcurrentSequences int32 = 8
	DefaultGatewayName                  = "kserve-ingress-gateway"
	DefaultGatewayNamespace             = "kserve"
	DefaultStepInterval                 = 5 * time.Minute
	DefaultProgressDeadline             = 30 * time.Minute
)

// DefaultCanarySteps are the canary traffic weights used when none are set.
var DefaultCanarySteps = []int32{10, 50, 100}

// ModelSpec identifies the model and its architecture.
// +kubebuilder:validation:XValidation:rule="has(self.catalogRef) != has(self.architecture)",message="exactly one of catalogRef or architecture must be set"
type ModelSpec struct {
	// Family is the model family.
	Family ModelFamily `json:"family"`

	// Name is the model name clients use in OpenAI-compatible requests, for
	// example meta-llama/Llama-3.1-8B-Instruct. It is passed to vLLM as
	// --served-model-name and to KServe as spec.model.name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// CatalogRef selects a built-in architecture entry.
	// +kubebuilder:validation:Enum=llama-3.1-8b;llama-3.1-70b;qwen2.5-7b;qwen2.5-14b;qwen2.5-72b
	// +optional
	CatalogRef string `json:"catalogRef,omitempty"`

	// Architecture describes a model that is not in the built-in catalog.
	// +optional
	Architecture *ModelArchitecture `json:"architecture,omitempty"`
}

// ModelArchitecture holds the dimensions the memory planner needs. The values
// come from the model's config.json.
type ModelArchitecture struct {
	// ParametersMillions is the total parameter count in millions, e.g. 8030
	// for an 8.03B model.
	// +kubebuilder:validation:Minimum=1
	ParametersMillions int64 `json:"parametersMillions"`

	// Layers is num_hidden_layers.
	// +kubebuilder:validation:Minimum=1
	Layers int32 `json:"layers"`

	// KVHeads is num_key_value_heads.
	// +kubebuilder:validation:Minimum=1
	KVHeads int32 `json:"kvHeads"`

	// HeadDim is the attention head dimension (hidden_size / num_attention_heads
	// unless head_dim is set explicitly).
	// +kubebuilder:validation:Minimum=1
	HeadDim int32 `json:"headDim"`

	// HiddenSize is hidden_size. Together with VocabSize it sizes the embedding
	// and LM head, which AWQ and GPTQ checkpoints keep in 16-bit precision.
	// +kubebuilder:validation:Minimum=1
	HiddenSize int32 `json:"hiddenSize"`

	// VocabSize is vocab_size.
	// +kubebuilder:validation:Minimum=1
	VocabSize int32 `json:"vocabSize"`

	// TiedEmbeddings is tie_word_embeddings.
	// +optional
	TiedEmbeddings bool `json:"tiedEmbeddings,omitempty"`

	// MaxContextLength is max_position_embeddings. gpuBudget.maxModelLen may
	// not exceed it.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxContextLength int32 `json:"maxContextLength,omitempty"`
}

// ModelVariant is one checkpoint of the model.
// +kubebuilder:validation:XValidation:rule="self.quantization != 'fp16' || !has(self.bits) || self.bits == 16",message="fp16 variants have 16 bits"
// +kubebuilder:validation:XValidation:rule="self.quantization != 'awq' || !has(self.bits) || self.bits == 4",message="awq variants have 4 bits"
// +kubebuilder:validation:XValidation:rule="self.quantization != 'gptq' || !has(self.bits) || self.bits == 4 || self.bits == 8",message="gptq variants have 4 or 8 bits"
type ModelVariant struct {
	// Quantization is the weight format.
	Quantization Quantization `json:"quantization"`

	// URI is the checkpoint location in a form KServe's storage initializer
	// understands, e.g. hf://hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4.
	// +kubebuilder:validation:Pattern=`^(hf|s3|gs|pvc|oci|https?)://.+`
	URI string `json:"uri"`

	// Bits is the weight width. It defaults to 16 for fp16 and 4 for awq and gptq.
	// +optional
	Bits int32 `json:"bits,omitempty"`
}

// GPUBudget bounds the GPU resources one replica may claim and the serving
// shape it must sustain.
type GPUBudget struct {
	// GPUType is the MIG-capable GPU model the replicas run on.
	// +kubebuilder:validation:Enum=A100-80GB;A100-40GB;H100-80GB
	GPUType string `json:"gpuType"`

	// MaxMemoryGiBPerReplica caps the usable memory of the MIG profile a
	// replica may claim. Without it any profile of the GPU may be chosen.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxMemoryGiBPerReplica *int32 `json:"maxMemoryGiBPerReplica,omitempty"`

	// MaxModelLen is the context length per sequence (vLLM --max-model-len).
	// +kubebuilder:validation:Minimum=256
	// +kubebuilder:default=8192
	// +optional
	MaxModelLen int32 `json:"maxModelLen,omitempty"`

	// MaxConcurrentSequences is the number of sequences a replica serves at
	// once (vLLM --max-num-seqs). The KV cache is sized so that all of them can
	// reach maxModelLen.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=8
	// +optional
	MaxConcurrentSequences int32 `json:"maxConcurrentSequences,omitempty"`
}

// RuntimeSpec configures the vLLM container.
type RuntimeSpec struct {
	// Image is the vLLM image. When empty, the image from KServe's
	// LLMInferenceServiceConfig is used.
	// +optional
	Image string `json:"image,omitempty"`

	// ExtraArgs are appended to the vLLM arguments the operator generates.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`

	// Env is added to the vLLM container, e.g. HF_TOKEN from a Secret.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`
}

// RolloutSpec configures how new revisions are rolled out.
type RolloutSpec struct {
	// Strategy is Canary or Recreate.
	// +kubebuilder:default=Canary
	// +optional
	Strategy RolloutStrategy `json:"strategy,omitempty"`

	// Steps are the canary traffic weights in percent, applied in order. They
	// must increase strictly and end at 100.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:Minimum=1
	// +kubebuilder:validation:items:Maximum=100
	// +kubebuilder:validation:XValidation:rule="self[size(self) - 1] == 100",message="the last step must be 100"
	// +kubebuilder:validation:XValidation:rule="self.all(i, self.filter(j, j == i).size() == 1) && self.isSorted()",message="steps must be strictly increasing"
	// +kubebuilder:default={10,50,100}
	// +optional
	Steps []int32 `json:"steps,omitempty"`

	// StepInterval is how long the canary must stay Ready at a step before it
	// moves to the next one (or, at the last step, is promoted).
	// +kubebuilder:default="5m"
	// +optional
	StepInterval *metav1.Duration `json:"stepInterval,omitempty"`

	// ProgressDeadline is how long a step may wait for the canary to be Ready
	// before the rollout is rolled back.
	// +kubebuilder:default="30m"
	// +optional
	ProgressDeadline *metav1.Duration `json:"progressDeadline,omitempty"`
}

// GatewayReference names a Gateway API Gateway.
type GatewayReference struct {
	// Name of the Gateway.
	// +kubebuilder:default=kserve-ingress-gateway
	// +optional
	Name string `json:"name,omitempty"`

	// Namespace of the Gateway.
	// +kubebuilder:default=kserve
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// RoutingSpec configures the HTTPRoute that splits traffic between revisions.
type RoutingSpec struct {
	// Gateway the HTTPRoute attaches to.
	// +kubebuilder:default={}
	// +optional
	Gateway GatewayReference `json:"gateway,omitempty"`

	// Hostnames of the HTTPRoute. Empty matches all hostnames of the Gateway.
	// +kubebuilder:validation:MaxItems=16
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`
}

// ModelDeploymentSpec defines the desired state of ModelDeployment.
type ModelDeploymentSpec struct {
	// Tenant owns the deployment; its TenantQuota bounds the MIG slices the
	// deployment may claim.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Tenant string `json:"tenant"`

	// Model identifies the model and its architecture.
	Model ModelSpec `json:"model"`

	// Variants are the available checkpoints. The planner picks the
	// highest-quality one that fits the GPU budget.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	Variants []ModelVariant `json:"variants"`

	// GPUBudget bounds the resources of one replica.
	GPUBudget GPUBudget `json:"gpuBudget"`

	// Replicas is the number of vLLM replicas, each claiming one MIG instance.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Runtime configures the vLLM container.
	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// Rollout configures how changes are rolled out.
	// +kubebuilder:default={}
	// +optional
	Rollout RolloutSpec `json:"rollout,omitempty"`

	// Routing configures the HTTPRoute in front of the revisions.
	// +kubebuilder:default={}
	// +optional
	Routing RoutingSpec `json:"routing,omitempty"`
}

// DeploymentPhase is a summary of the deployment state.
// +kubebuilder:validation:Enum=Pending;Progressing;Ready;Failed
type DeploymentPhase string

const (
	PhasePending     DeploymentPhase = "Pending"
	PhaseProgressing DeploymentPhase = "Progressing"
	PhaseReady       DeploymentPhase = "Ready"
	PhaseFailed      DeploymentPhase = "Failed"
)

// Condition types of ModelDeployment.
const (
	// ConditionPlanned is True when a variant and MIG profile fit the budget.
	ConditionPlanned = "Planned"
	// ConditionReady is True when the stable revision reports Ready.
	ConditionReady = "Ready"
	// ConditionProgressing is True while a canary rollout is in progress.
	ConditionProgressing = "Progressing"
	// ConditionQuotaExceeded is True when a requested increase was not applied
	// because it would exceed the tenant's quota.
	ConditionQuotaExceeded = "QuotaExceeded"
	// ConditionRolledBack is True when the last canary was rolled back.
	ConditionRolledBack = "RolledBack"
)

// SelectedVariant is the variant the planner chose.
type SelectedVariant struct {
	Quantization Quantization `json:"quantization"`
	Bits         int32        `json:"bits"`
	URI          string       `json:"uri"`
}

// MemoryEstimate is the planner's per-replica memory estimate in MiB.
type MemoryEstimate struct {
	WeightsMiB  int64 `json:"weightsMiB"`
	KVCacheMiB  int64 `json:"kvCacheMiB"`
	OverheadMiB int64 `json:"overheadMiB"`
	TotalMiB    int64 `json:"totalMiB"`
	// ProfileMiB is the usable memory of the chosen MIG profile.
	ProfileMiB int64 `json:"profileMiB"`
}

// SliceAllocation is the MIG capacity one revision holds.
type SliceAllocation struct {
	// Revision is the revision hash.
	Revision string `json:"revision"`
	// Profile is the MIG profile, e.g. 1g.20gb.
	Profile string `json:"profile"`
	// Count is the number of MIG instances (one per replica).
	Count int32 `json:"count"`
}

// ModelDeploymentStatus defines the observed state of ModelDeployment.
type ModelDeploymentStatus struct {
	// ObservedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Phase summarizes the state.
	// +optional
	Phase DeploymentPhase `json:"phase,omitempty"`

	// SelectedVariant is the variant planned for the current spec. During a
	// canary rollout this is the canary's variant.
	// +optional
	SelectedVariant *SelectedVariant `json:"selectedVariant,omitempty"`

	// MIGProfile is the MIG profile planned for the current spec.
	// +optional
	MIGProfile string `json:"migProfile,omitempty"`

	// MemoryEstimate is the per-replica estimate for the current spec.
	// +optional
	MemoryEstimate *MemoryEstimate `json:"memoryEstimate,omitempty"`

	// ReplicasPerGPU is how many replicas of the current plan fit on one GPU.
	// +optional
	ReplicasPerGPU int32 `json:"replicasPerGPU,omitempty"`

	// StableRevision is the revision serving production traffic.
	// +optional
	StableRevision string `json:"stableRevision,omitempty"`

	// CanaryRevision is the revision being rolled out.
	// +optional
	CanaryRevision string `json:"canaryRevision,omitempty"`

	// CanaryWeight is the percentage of traffic routed to the canary.
	// +optional
	CanaryWeight int32 `json:"canaryWeight,omitempty"`

	// CanaryStep is the index into spec.rollout.steps, or -1 while the canary
	// starts up and receives no traffic.
	// +optional
	CanaryStep int32 `json:"canaryStep,omitempty"`

	// CanaryStepStartTime is when the current canary step began.
	// +optional
	CanaryStepStartTime *metav1.Time `json:"canaryStepStartTime,omitempty"`

	// RolledBackRevision is the last revision that was rolled back. It is not
	// retried until the spec changes to a different revision.
	// +optional
	RolledBackRevision string `json:"rolledBackRevision,omitempty"`

	// AllocatedSlices are the MIG instances claimed per revision. They count
	// against the tenant's quota.
	// +listType=map
	// +listMapKey=revision
	// +optional
	AllocatedSlices []SliceAllocation `json:"allocatedSlices,omitempty"`

	// Conditions are Planned, Ready, Progressing, QuotaExceeded and RolledBack.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mdep
// +kubebuilder:printcolumn:name="Tenant",type=string,JSONPath=`.spec.tenant`
// +kubebuilder:printcolumn:name="Variant",type=string,JSONPath=`.status.selectedVariant.quantization`
// +kubebuilder:printcolumn:name="MIG",type=string,JSONPath=`.status.migProfile`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Canary",type=integer,JSONPath=`.status.canaryWeight`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelDeployment rolls out an LLM on KServe, choosing a quantized variant and
// a MIG profile that fit the GPU budget.
type ModelDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ModelDeploymentSpec   `json:"spec"`
	Status ModelDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ModelDeploymentList contains a list of ModelDeployment.
type ModelDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelDeployment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelDeployment{}, &ModelDeploymentList{})
}
