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

package render

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
)

func testDeployment() *servingv1alpha1.ModelDeployment {
	return &servingv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "llama", Namespace: "team-a"},
		Spec: servingv1alpha1.ModelDeploymentSpec{
			Tenant: "team-a",
			Routing: servingv1alpha1.RoutingSpec{
				Gateway:   servingv1alpha1.GatewayReference{Name: "gw", Namespace: "gw-ns"},
				Hostnames: []string{"llm.example.com"},
			},
		},
	}
}

func testTemplate() Template {
	return Template{
		ModelName:            "meta-llama/Llama-3.1-8B-Instruct",
		Variant:              servingv1alpha1.SelectedVariant{Quantization: servingv1alpha1.QuantizationAWQ, Bits: 4, URI: "hf://org/llama-awq"},
		GPUType:              "A100-80GB",
		MIGProfile:           "1g.20gb",
		MaxModelLen:          8192,
		MaxNumSeqs:           8,
		GPUMemoryUtilization: "0.90",
		Image:                "vllm/vllm-openai:v0.10.1",
		ExtraArgs:            []string{"--enable-prefix-caching"},
		Env:                  []corev1.EnvVar{{Name: "HF_HOME", Value: "/tmp/hf"}},
	}
}

func TestTemplateHash(t *testing.T) {
	a := testTemplate()
	if a.Hash() != testTemplate().Hash() {
		t.Fatal("hash is not deterministic")
	}
	if len(a.Hash()) != 8 {
		t.Fatalf("hash %q should be 8 characters", a.Hash())
	}
	b := testTemplate()
	b.MIGProfile = "3g.40gb"
	c := testTemplate()
	c.Image = "vllm/vllm-openai:v0.11.0"
	if a.Hash() == b.Hash() || a.Hash() == c.Hash() {
		t.Error("changing the profile or image must change the revision")
	}
}

func TestVLLMArgs(t *testing.T) {
	tests := []struct {
		quant servingv1alpha1.Quantization
		want  string
	}{
		{servingv1alpha1.QuantizationAWQ, "awq_marlin"},
		{servingv1alpha1.QuantizationGPTQ, "gptq_marlin"},
		{servingv1alpha1.QuantizationFP16, ""},
	}
	for _, tt := range tests {
		tmpl := testTemplate()
		tmpl.Variant.Quantization = tt.quant
		args := VLLMArgs(tmpl)
		i := slices.Index(args, "--quantization")
		switch {
		case tt.want == "" && i >= 0:
			t.Errorf("%s: unexpected --quantization in %v", tt.quant, args)
		case tt.want != "" && (i < 0 || args[i+1] != tt.want):
			t.Errorf("%s: want --quantization %s in %v", tt.quant, tt.want, args)
		}
		for _, pair := range [][2]string{
			{"--max-model-len", "8192"}, {"--max-num-seqs", "8"}, {"--gpu-memory-utilization", "0.90"},
			{"--served-model-name", "meta-llama/Llama-3.1-8B-Instruct"},
		} {
			j := slices.Index(args, pair[0])
			if j < 0 || args[j+1] != pair[1] {
				t.Errorf("%s: want %s %s in %v", tt.quant, pair[0], pair[1], args)
			}
		}
		if args[len(args)-1] != "--enable-prefix-caching" {
			t.Errorf("extra args must come last: %v", args)
		}
	}
}

func TestClaimTemplate(t *testing.T) {
	rct := ClaimTemplate(testDeployment(), "abc12345", testTemplate())
	if rct.Name != "llama-abc12345-mig" || rct.Namespace != "team-a" {
		t.Errorf("name = %s/%s", rct.Namespace, rct.Name)
	}
	reqs := rct.Spec.Spec.Devices.Requests
	if len(reqs) != 1 || reqs[0].Exactly == nil {
		t.Fatalf("want one exact request, got %+v", reqs)
	}
	ex := reqs[0].Exactly
	if ex.DeviceClassName != "mig.nvidia.com" || ex.Count != 1 || ex.AllocationMode != resourcev1.DeviceAllocationModeExactCount {
		t.Errorf("request = %+v", ex)
	}
	var exprs []string
	for _, s := range ex.Selectors {
		exprs = append(exprs, s.CEL.Expression)
	}
	want := []string{
		"device.attributes['gpu.nvidia.com'].profile == '1g.20gb'",
		"device.attributes['gpu.nvidia.com'].productName.matches('A100.*80GB')",
	}
	if !slices.Equal(exprs, want) {
		t.Errorf("selectors = %q, want %q", exprs, want)
	}
	if rct.Annotations[AnnotationSpecHash] == "" || rct.Labels[LabelRevision] != "abc12345" {
		t.Errorf("metadata = %+v", rct.ObjectMeta)
	}
}

func TestLLMInferenceService(t *testing.T) {
	u := LLMInferenceService(testDeployment(), "abc12345", testTemplate(), 4)
	if u.GetAPIVersion() != "serving.kserve.io/v1alpha1" || u.GetKind() != "LLMInferenceService" || u.GetName() != "llama-abc12345" {
		t.Fatalf("object = %s %s %s", u.GetAPIVersion(), u.GetKind(), u.GetName())
	}
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedString(u.Object, path...)
		return v
	}
	if got := str("spec", "model", "uri"); got != "hf://org/llama-awq" {
		t.Errorf("model.uri = %s", got)
	}
	if got := str("spec", "model", "name"); got != "meta-llama/Llama-3.1-8B-Instruct" {
		t.Errorf("model.name = %s", got)
	}
	if n, _, _ := unstructured.NestedInt64(u.Object, "spec", "replicas"); n != 4 {
		t.Errorf("replicas = %d", n)
	}
	if _, found, _ := unstructured.NestedMap(u.Object, "spec", "router", "scheduler"); !found {
		t.Error("router.scheduler missing")
	}
	routes, _, _ := unstructured.NestedSlice(u.Object, "spec", "router", "route", "http", "refs")
	if len(routes) != 1 || routes[0].(map[string]any)["name"] != "llama" {
		t.Errorf("router.route.http.refs = %v", routes)
	}
	containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "containers")
	if len(containers) != 1 {
		t.Fatalf("containers = %v", containers)
	}
	c := containers[0].(map[string]any)
	if c["name"] != "main" || c["image"] != "vllm/vllm-openai:v0.10.1" {
		t.Errorf("container = %v", c)
	}
	claims, _, _ := unstructured.NestedSlice(c, "resources", "claims")
	if len(claims) != 1 || claims[0].(map[string]any)["name"] != ClaimName {
		t.Errorf("container claims = %v", claims)
	}
	env, _, _ := unstructured.NestedSlice(c, "env")
	if len(env) != 1 || env[0].(map[string]any)["name"] != "HF_HOME" {
		t.Errorf("env = %v", env)
	}
	podClaims, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "resourceClaims")
	if len(podClaims) != 1 || podClaims[0].(map[string]any)["resourceClaimTemplateName"] != "llama-abc12345-mig" {
		t.Errorf("pod resourceClaims = %v", podClaims)
	}
	if u.GetAnnotations()[AnnotationSpecHash] != SpecHash(u.Object["spec"]) {
		t.Error("spec-hash annotation does not match the spec")
	}
}

func TestHTTPRoute(t *testing.T) {
	u := HTTPRoute(testDeployment(), []Backend{{Revision: "aaa", Weight: 90}, {Revision: "bbb", Weight: 10}}, DefaultInferencePoolGroup)
	rules, _, _ := unstructured.NestedSlice(u.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("rules = %v", rules)
	}
	refs := rules[0].(map[string]any)["backendRefs"].([]any)
	if len(refs) != 2 {
		t.Fatalf("backendRefs = %v", refs)
	}
	for i, want := range []struct {
		name   string
		weight int64
	}{{"llama-aaa-inference-pool", 90}, {"llama-bbb-inference-pool", 10}} {
		ref := refs[i].(map[string]any)
		if ref["name"] != want.name || ref["weight"] != want.weight || ref["kind"] != "InferencePool" || ref["group"] != DefaultInferencePoolGroup {
			t.Errorf("backendRef %d = %v", i, ref)
		}
	}
	parents, _, _ := unstructured.NestedSlice(u.Object, "spec", "parentRefs")
	if p := parents[0].(map[string]any); p["name"] != "gw" || p["namespace"] != "gw-ns" {
		t.Errorf("parentRefs = %v", parents)
	}
	hosts, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "hostnames")
	if !slices.Equal(hosts, []string{"llm.example.com"}) {
		t.Errorf("hostnames = %v", hosts)
	}
	match := rules[0].(map[string]any)["matches"].([]any)[0].(map[string]any)["path"].(map[string]any)
	if match["value"] != "/team-a/llama" {
		t.Errorf("path match = %v", match)
	}
}

func TestIsReady(t *testing.T) {
	mk := func(conds ...any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"conditions": conds}}}
	}
	tests := []struct {
		name string
		u    *unstructured.Unstructured
		want bool
	}{
		{"no status", &unstructured.Unstructured{Object: map[string]any{}}, false},
		{"ready", mk(map[string]any{"type": "Ready", "status": "True"}), true},
		{"not ready", mk(map[string]any{"type": "Ready", "status": "False"}), false},
		{"other condition only", mk(map[string]any{"type": "WorkloadReady", "status": "True"}), false},
	}
	for _, tt := range tests {
		if got := IsReady(tt.u); got != tt.want {
			t.Errorf("%s: IsReady = %v, want %v", tt.name, got, tt.want)
		}
	}
}
