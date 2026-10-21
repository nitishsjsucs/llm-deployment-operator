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

package v1alpha1_test

// These tests run the generated CRDs through the same validation code the
// API server uses (CRD validation including CEL compilation and cost
// estimation, OpenAPI schema validation, defaulting and CEL rule evaluation),
// without a cluster.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	schemavalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

const (
	crdDir    = "../../config/crd/bases"
	sampleDir = "../../config/samples"
)

type crdSchema struct {
	structural *structuralschema.Structural
	schema     schemavalidation.SchemaValidator
	cel        *cel.Validator
}

func loadCRD(t *testing.T, file string) *apiextensions.CustomResourceDefinition {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(crdDir, file))
	if err != nil {
		t.Fatal(err)
	}
	v1 := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.UnmarshalStrict(data, v1); err != nil {
		t.Fatal(err)
	}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(v1)
	crd := &apiextensions.CustomResourceDefinition{}
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(v1, crd, nil); err != nil {
		t.Fatal(err)
	}
	return crd
}

func schemaFor(t *testing.T, file string) crdSchema {
	t.Helper()
	crd := loadCRD(t, file)
	props := crd.Spec.Versions[0].Schema
	if props == nil {
		props = crd.Spec.Validation
	}
	ss, err := structuralschema.NewStructural(props.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	sv, _, err := schemavalidation.NewSchemaValidator(props.OpenAPIV3Schema)
	if err != nil {
		t.Fatal(err)
	}
	return crdSchema{structural: ss, schema: sv, cel: cel.NewValidator(ss, true, celconfig.PerCallLimit)}
}

// admit defaults obj and returns the validation errors the API server would report.
func (s crdSchema) admit(obj map[string]any) field.ErrorList {
	structuraldefaulting.Default(obj, s.structural)
	errs := schemavalidation.ValidateCustomResource(nil, obj, s.schema)
	celErrs, _ := s.cel.Validate(context.Background(), nil, s.structural, obj, nil, celconfig.RuntimeCELCostBudget)
	return append(errs, celErrs...)
}

func loadSample(t *testing.T, file string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(sampleDir, file))
	if err != nil {
		t.Fatal(err)
	}
	// Decode the way the API server does: integral numbers become int64.
	js, err := yaml.YAMLToJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	obj := map[string]any{}
	if err := utiljson.Unmarshal(js, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestCRDsPassAPIServerValidation(t *testing.T) {
	for _, file := range []string{"serving.llmops.dev_modeldeployments.yaml", "serving.llmops.dev_tenantquotas.yaml"} {
		if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), loadCRD(t, file)); len(errs) > 0 {
			t.Errorf("%s: %v", file, errs.ToAggregate())
		}
	}
}

func TestSamplesAreValid(t *testing.T) {
	md := schemaFor(t, "serving.llmops.dev_modeldeployments.yaml")
	tq := schemaFor(t, "serving.llmops.dev_tenantquotas.yaml")
	for file, s := range map[string]crdSchema{
		"serving_v1alpha1_modeldeployment_llama.yaml": md,
		"serving_v1alpha1_modeldeployment_qwen.yaml":  md,
		"serving_v1alpha1_tenantquota.yaml":           tq,
	} {
		if errs := s.admit(loadSample(t, file)); len(errs) > 0 {
			t.Errorf("%s: %v", file, errs.ToAggregate())
		}
	}
}

func TestDefaults(t *testing.T) {
	obj := loadSample(t, "serving_v1alpha1_modeldeployment_qwen.yaml")
	if errs := schemaFor(t, "serving.llmops.dev_modeldeployments.yaml").admit(obj); len(errs) > 0 {
		t.Fatal(errs.ToAggregate())
	}
	str := func(path ...string) string {
		v, _, _ := unstructured.NestedFieldNoCopy(obj, path...)
		s, _ := v.(string)
		return s
	}
	for path, want := range map[string]string{
		"spec.rollout.strategy":          "Canary",
		"spec.rollout.stepInterval":      "5m",
		"spec.rollout.progressDeadline":  "30m",
		"spec.routing.gateway.name":      "kserve-ingress-gateway",
		"spec.routing.gateway.namespace": "kserve",
	} {
		if got := str(strings.Split(path, ".")...); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	steps, _, _ := unstructured.NestedSlice(obj, "spec", "rollout", "steps")
	if len(steps) != 3 {
		t.Errorf("default steps = %v", steps)
	}
}

func TestValidationRules(t *testing.T) {
	md := schemaFor(t, "serving.llmops.dev_modeldeployments.yaml")
	tq := schemaFor(t, "serving.llmops.dev_tenantquotas.yaml")
	set := func(obj map[string]any, value any, path ...string) {
		if err := unstructured.SetNestedField(obj, value, path...); err != nil {
			t.Fatal(err)
		}
	}
	variant := func(obj map[string]any, i int, key string, value any) {
		vs, _, _ := unstructured.NestedSlice(obj, "spec", "variants")
		vs[i].(map[string]any)[key] = value
		set(obj, vs, "spec", "variants")
	}
	tests := []struct {
		name    string
		schema  crdSchema
		sample  string
		mutate  func(map[string]any)
		wantErr string
	}{
		{
			name: "catalogRef and architecture are exclusive", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate: func(o map[string]any) {
				set(o, map[string]any{"parametersMillions": int64(8030), "layers": int64(32), "kvHeads": int64(8), "headDim": int64(128), "hiddenSize": int64(4096), "vocabSize": int64(128256)}, "spec", "model", "architecture")
			},
			wantErr: "exactly one of catalogRef or architecture must be set",
		},
		{
			name: "awq is 4-bit only", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { variant(o, 1, "bits", int64(8)) },
			wantErr: "awq variants have 4 bits",
		},
		{
			name: "gptq allows 8 bits", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate: func(o map[string]any) { variant(o, 2, "bits", int64(8)) },
		},
		{
			name: "steps must end at 100", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { set(o, []any{int64(10), int64(50)}, "spec", "rollout", "steps") },
			wantErr: "the last step must be 100",
		},
		{
			name: "steps must increase", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { set(o, []any{int64(50), int64(10), int64(100)}, "spec", "rollout", "steps") },
			wantErr: "steps must be strictly increasing",
		},
		{
			name: "steps must not repeat", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { set(o, []any{int64(50), int64(50), int64(100)}, "spec", "rollout", "steps") },
			wantErr: "steps must be strictly increasing",
		},
		{
			name: "unknown GPU type", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { set(o, "T4", "spec", "gpuBudget", "gpuType") },
			wantErr: "spec.gpuBudget.gpuType",
		},
		{
			name: "unknown catalog entry", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { set(o, "llama-2-7b", "spec", "model", "catalogRef") },
			wantErr: "spec.model.catalogRef",
		},
		{
			name: "variant URI scheme", schema: md, sample: "serving_v1alpha1_modeldeployment_llama.yaml",
			mutate:  func(o map[string]any) { variant(o, 0, "uri", "ftp://models/llama") },
			wantErr: "spec.variants[0].uri",
		},
		{
			name: "quota profile name", schema: tq, sample: "serving_v1alpha1_tenantquota.yaml",
			mutate: func(o map[string]any) {
				set(o, []any{map[string]any{"profile": "big", "max": int64(1)}}, "spec", "migSlices")
			},
			wantErr: "spec.migSlices[0].profile",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := loadSample(t, tt.sample)
			tt.mutate(obj)
			errs := tt.schema.admit(obj)
			agg := errs.ToAggregate()
			switch {
			case tt.wantErr == "" && agg != nil:
				t.Errorf("unexpected errors: %v", agg)
			case tt.wantErr != "" && (agg == nil || !strings.Contains(agg.Error(), tt.wantErr)):
				t.Errorf("errors = %v, want one containing %q", agg, tt.wantErr)
			}
		})
	}
}
