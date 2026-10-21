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

// Package render builds the Kubernetes objects for a ModelDeployment
// revision: a DRA ResourceClaimTemplate, a KServe LLMInferenceService and a
// Gateway API HTTPRoute.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
)

const (
	// LabelDeployment is set on every child object to the ModelDeployment name.
	LabelDeployment = "serving.llmops.dev/deployment"
	// LabelRevision is set on per-revision child objects.
	LabelRevision = "serving.llmops.dev/revision"
	// LabelTenant is set on every child object to the tenant.
	LabelTenant = "serving.llmops.dev/tenant"
	// AnnotationSpecHash records the hash of the spec the operator last applied.
	AnnotationSpecHash = "serving.llmops.dev/spec-hash"

	// MIGDeviceClass is the DeviceClass the NVIDIA DRA driver creates for MIG devices.
	MIGDeviceClass = "mig.nvidia.com"
	// NVIDIADriverDomain qualifies the device attributes the NVIDIA DRA driver publishes.
	NVIDIADriverDomain = "gpu.nvidia.com"

	// ClaimName is the name of the pod-level resource claim.
	ClaimName = "mig"
	// MainContainer is the name of the vLLM container in the LLMInferenceService template.
	MainContainer = "main"
	// ModelMountPath is where KServe's storage initializer places the model.
	ModelMountPath = "/mnt/models"
	// VLLMPort is the port vLLM listens on.
	VLLMPort = 8000

	// InferencePoolSuffix is appended to the LLMInferenceService name by KServe
	// when it creates the InferencePool for the scheduler.
	InferencePoolSuffix = "-inference-pool"
	// DefaultInferencePoolGroup is the API group of the Gateway API Inference
	// Extension InferencePool.
	DefaultInferencePoolGroup = "inference.networking.k8s.io"
)

var (
	// LLMInferenceServiceGVK is KServe's LLMInferenceService.
	LLMInferenceServiceGVK = schema.GroupVersionKind{Group: "serving.kserve.io", Version: "v1alpha1", Kind: "LLMInferenceService"}
	// HTTPRouteGVK is the Gateway API HTTPRoute.
	HTTPRouteGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1", Kind: "HTTPRoute"}
	// ResourceClaimTemplateGVK is the DRA ResourceClaimTemplate.
	ResourceClaimTemplateGVK = resourcev1.SchemeGroupVersion.WithKind("ResourceClaimTemplate")
)

// Template is everything that defines a revision. Changing any field produces
// a new revision and, with the Canary strategy, a canary rollout. Replicas are
// deliberately not part of it: scaling happens in place.
type Template struct {
	ModelName   string                          `json:"modelName"`
	Variant     servingv1alpha1.SelectedVariant `json:"variant"`
	GPUType     string                          `json:"gpuType"`
	MIGProfile  string                          `json:"migProfile"`
	MaxModelLen int32                           `json:"maxModelLen"`
	MaxNumSeqs  int32                           `json:"maxNumSeqs"`
	// GPUMemoryUtilization is formatted as passed to vLLM, e.g. "0.90".
	GPUMemoryUtilization string          `json:"gpuMemoryUtilization"`
	Image                string          `json:"image,omitempty"`
	ExtraArgs            []string        `json:"extraArgs,omitempty"`
	Env                  []corev1.EnvVar `json:"env,omitempty"`
}

// Hash is the revision identifier: the first 8 hex characters of the SHA-256
// of the template's JSON encoding.
func (t Template) Hash() string {
	return hashJSON(t)[:8]
}

// SpecHash hashes an object's spec for the spec-hash annotation.
func SpecHash(spec any) string {
	return hashJSON(spec)[:16]
}

func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Templates and specs are plain data; marshalling cannot fail.
		panic(fmt.Sprintf("hashing %T: %v", v, err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// LLMInferenceServiceName is the name of a revision's LLMInferenceService.
func LLMInferenceServiceName(deployment, revision string) string {
	return deployment + "-" + revision
}

// ClaimTemplateName is the name of a revision's ResourceClaimTemplate.
func ClaimTemplateName(deployment, revision string) string {
	return deployment + "-" + revision + "-mig"
}

// ControllerRevisionName is the name of the ControllerRevision that stores a
// revision's Template.
func ControllerRevisionName(deployment, revision string) string {
	return deployment + "-" + revision
}

// RouteName is the name of the deployment's HTTPRoute.
func RouteName(deployment string) string { return deployment }

// Labels returns the labels for a child object; revision may be empty.
func Labels(md *servingv1alpha1.ModelDeployment, revision string) map[string]string {
	l := map[string]string{
		LabelDeployment: md.Name,
		LabelTenant:     md.Spec.Tenant,
	}
	if revision != "" {
		l[LabelRevision] = revision
	}
	return l
}

// QuantizationArg returns the vLLM --quantization value. All supported GPUs
// are Ampere or newer, so the Marlin kernels are used; passing plain "awq" or
// "gptq" would force vLLM onto the slower non-Marlin kernels.
func QuantizationArg(q servingv1alpha1.Quantization) string {
	switch q {
	case servingv1alpha1.QuantizationAWQ:
		return "awq_marlin"
	case servingv1alpha1.QuantizationGPTQ:
		return "gptq_marlin"
	}
	return ""
}

// VLLMArgs returns the vLLM command-line arguments for the template.
func VLLMArgs(t Template) []string {
	args := []string{
		"--served-model-name", t.ModelName,
		"--port", strconv.Itoa(VLLMPort),
	}
	if q := QuantizationArg(t.Variant.Quantization); q != "" {
		args = append(args, "--quantization", q)
	}
	args = append(args,
		"--max-model-len", strconv.Itoa(int(t.MaxModelLen)),
		"--max-num-seqs", strconv.Itoa(int(t.MaxNumSeqs)),
		"--gpu-memory-utilization", t.GPUMemoryUtilization,
	)
	return append(args, t.ExtraArgs...)
}

// MIGSelectors are the CEL expressions that pin a claim to one MIG profile on
// one GPU model, using the attributes the NVIDIA DRA driver publishes for MIG
// devices.
func MIGSelectors(t Template) []string {
	sel := []string{
		fmt.Sprintf("device.attributes['%s'].profile == '%s'", NVIDIADriverDomain, t.MIGProfile),
	}
	if gpu, ok := packing.LookupGPU(t.GPUType); ok && gpu.ProductNamePattern != "" {
		sel = append(sel, fmt.Sprintf("device.attributes['%s'].productName.matches('%s')", NVIDIADriverDomain, gpu.ProductNamePattern))
	}
	return sel
}

// ClaimTemplate returns the ResourceClaimTemplate from which every replica of
// the revision gets a claim for one MIG instance.
func ClaimTemplate(md *servingv1alpha1.ModelDeployment, revision string, t Template) *resourcev1.ResourceClaimTemplate {
	var selectors []resourcev1.DeviceSelector
	for _, expr := range MIGSelectors(t) {
		selectors = append(selectors, resourcev1.DeviceSelector{CEL: &resourcev1.CELDeviceSelector{Expression: expr}})
	}
	spec := resourcev1.ResourceClaimTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: Labels(md, revision)},
		Spec: resourcev1.ResourceClaimSpec{
			Devices: resourcev1.DeviceClaim{
				Requests: []resourcev1.DeviceRequest{{
					Name: ClaimName,
					Exactly: &resourcev1.ExactDeviceRequest{
						DeviceClassName: MIGDeviceClass,
						AllocationMode:  resourcev1.DeviceAllocationModeExactCount,
						Count:           1,
						Selectors:       selectors,
					},
				}},
			},
		},
	}
	rct := &resourcev1.ResourceClaimTemplate{
		TypeMeta: metav1.TypeMeta{APIVersion: ResourceClaimTemplateGVK.GroupVersion().String(), Kind: ResourceClaimTemplateGVK.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name:        ClaimTemplateName(md.Name, revision),
			Namespace:   md.Namespace,
			Labels:      Labels(md, revision),
			Annotations: map[string]string{AnnotationSpecHash: SpecHash(spec)},
		},
		Spec: spec,
	}
	return rct
}

// LLMInferenceService returns the KServe LLMInferenceService of a revision.
// KServe creates the workload, the InferencePool and the endpoint picker
// (spec.router.scheduler); routing goes through the operator's HTTPRoute,
// which is referenced from spec.router.route so KServe does not create its own.
func LLMInferenceService(md *servingv1alpha1.ModelDeployment, revision string, t Template, replicas int32) *unstructured.Unstructured {
	container := map[string]any{
		"name":    MainContainer,
		"command": []any{"vllm", "serve", ModelMountPath},
		"args":    toAnySlice(VLLMArgs(t)),
		"resources": map[string]any{
			"claims": []any{map[string]any{"name": ClaimName}},
		},
	}
	if t.Image != "" {
		container["image"] = t.Image
	}
	if len(t.Env) > 0 {
		container["env"] = mustToUnstructuredSlice(t.Env)
	}

	gateway := md.Spec.Routing.Gateway
	spec := map[string]any{
		"model": map[string]any{
			"uri":  t.Variant.URI,
			"name": t.ModelName,
		},
		"replicas": int64(replicas),
		"router": map[string]any{
			"scheduler": map[string]any{},
			"gateway": map[string]any{
				"refs": []any{map[string]any{"name": gateway.Name, "namespace": gateway.Namespace}},
			},
			"route": map[string]any{
				"http": map[string]any{
					"refs": []any{map[string]any{"name": RouteName(md.Name)}},
				},
			},
		},
		"template": map[string]any{
			"containers": []any{container},
			"resourceClaims": []any{map[string]any{
				"name":                      ClaimName,
				"resourceClaimTemplateName": ClaimTemplateName(md.Name, revision),
			}},
		},
	}

	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(LLMInferenceServiceGVK)
	u.SetName(LLMInferenceServiceName(md.Name, revision))
	u.SetNamespace(md.Namespace)
	u.SetLabels(Labels(md, revision))
	u.SetAnnotations(map[string]string{AnnotationSpecHash: SpecHash(spec)})
	return u
}

// Backend is one weighted target of the HTTPRoute.
type Backend struct {
	Revision string
	Weight   int32
}

// HTTPRoute returns the route that splits traffic between the revisions'
// InferencePools. Requests to /<namespace>/<deployment>/... are forwarded with
// that prefix stripped, so the OpenAI-compatible paths stay the same across
// revisions.
func HTTPRoute(md *servingv1alpha1.ModelDeployment, backends []Backend, poolGroup string) *unstructured.Unstructured {
	refs := make([]any, 0, len(backends))
	for _, b := range backends {
		refs = append(refs, map[string]any{
			"group":  poolGroup,
			"kind":   "InferencePool",
			"name":   LLMInferenceServiceName(md.Name, b.Revision) + InferencePoolSuffix,
			"weight": int64(b.Weight),
		})
	}
	gateway := md.Spec.Routing.Gateway
	spec := map[string]any{
		"parentRefs": []any{map[string]any{
			"group":     "gateway.networking.k8s.io",
			"kind":      "Gateway",
			"name":      gateway.Name,
			"namespace": gateway.Namespace,
		}},
		"rules": []any{map[string]any{
			"matches": []any{map[string]any{
				"path": map[string]any{"type": "PathPrefix", "value": fmt.Sprintf("/%s/%s", md.Namespace, md.Name)},
			}},
			"filters": []any{map[string]any{
				"type": "URLRewrite",
				"urlRewrite": map[string]any{
					"path": map[string]any{"type": "ReplacePrefixMatch", "replacePrefixMatch": "/"},
				},
			}},
			"backendRefs": refs,
		}},
	}
	if len(md.Spec.Routing.Hostnames) > 0 {
		spec["hostnames"] = toAnySlice(md.Spec.Routing.Hostnames)
	}

	u := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	u.SetGroupVersionKind(HTTPRouteGVK)
	u.SetName(RouteName(md.Name))
	u.SetNamespace(md.Namespace)
	u.SetLabels(Labels(md, ""))
	u.SetAnnotations(map[string]string{AnnotationSpecHash: SpecHash(spec)})
	return u
}

// IsReady reports whether an object's status has a Ready condition that is True.
func IsReady(u *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func mustToUnstructuredSlice(env []corev1.EnvVar) []any {
	out := make([]any, 0, len(env))
	for i := range env {
		m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&env[i])
		if err != nil {
			panic(fmt.Sprintf("converting env var %q: %v", env[i].Name, err))
		}
		out = append(out, m)
	}
	return out
}
