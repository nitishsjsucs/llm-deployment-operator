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

package controller

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/planner"
)

func TestArchitectureFor(t *testing.T) {
	custom := &servingv1alpha1.ModelArchitecture{
		ParametersMillions: 7616, Layers: 28, KVHeads: 4, HeadDim: 128, HiddenSize: 3584, VocabSize: 152064, MaxContextLength: 32768,
	}
	tests := []struct {
		name    string
		spec    servingv1alpha1.ModelSpec
		wantErr bool
		check   func(planner.Architecture) bool
	}{
		{
			name:  "catalog entry",
			spec:  servingv1alpha1.ModelSpec{Family: servingv1alpha1.ModelFamilyQwen, CatalogRef: "qwen2.5-7b"},
			check: func(a planner.Architecture) bool { return a.Layers == 28 && a.KVHeads == 4 },
		},
		{
			name:    "family mismatch",
			spec:    servingv1alpha1.ModelSpec{Family: servingv1alpha1.ModelFamilyLlama, CatalogRef: "qwen2.5-7b"},
			wantErr: true,
		},
		{
			name:    "unknown entry",
			spec:    servingv1alpha1.ModelSpec{Family: servingv1alpha1.ModelFamilyLlama, CatalogRef: "llama-9"},
			wantErr: true,
		},
		{
			name:  "inline architecture",
			spec:  servingv1alpha1.ModelSpec{Family: servingv1alpha1.ModelFamilyQwen, Architecture: custom},
			check: func(a planner.Architecture) bool { return a.Params == 7_616_000_000 && a.MaxContextLen == 32768 },
		},
		{
			name:    "neither",
			spec:    servingv1alpha1.ModelSpec{Family: servingv1alpha1.ModelFamilyQwen},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		a, err := architectureFor(tt.spec)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if tt.check != nil && !tt.check(a) {
			t.Errorf("%s: unexpected architecture %+v", tt.name, a)
		}
	}
}

func TestWithDefaults(t *testing.T) {
	s := withDefaults(servingv1alpha1.ModelDeploymentSpec{})
	switch {
	case *s.Replicas != 1,
		s.GPUBudget.MaxModelLen != 8192,
		s.GPUBudget.MaxConcurrentSequences != 8,
		s.Rollout.Strategy != servingv1alpha1.RolloutStrategyCanary,
		len(s.Rollout.Steps) != 3,
		s.Rollout.StepInterval.Duration != servingv1alpha1.DefaultStepInterval,
		s.Rollout.ProgressDeadline.Duration != servingv1alpha1.DefaultProgressDeadline,
		s.Routing.Gateway.Name != "kserve-ingress-gateway",
		s.Routing.Gateway.Namespace != "kserve":
		t.Errorf("defaults not applied: %+v", s)
	}
	// The default steps must not alias the package-level slice.
	s.Rollout.Steps[0] = 99
	if servingv1alpha1.DefaultCanarySteps[0] != 10 {
		t.Error("withDefaults aliases DefaultCanarySteps")
	}
}

func TestRevisionIgnoresReplicas(t *testing.T) {
	a := withDefaults(llamaDeployment("x", "ns", 1).Spec)
	b := withDefaults(llamaDeployment("x", "ns", 8).Spec)
	pa, err := planFor(a)
	if err != nil {
		t.Fatal(err)
	}
	pb, err := planFor(b)
	if err != nil {
		t.Fatal(err)
	}
	if templateFor(a, pa).Hash() != templateFor(b, pb).Hash() {
		t.Error("scaling must not create a new revision")
	}
}

func TestPlanForRejectsBadSteps(t *testing.T) {
	s := withDefaults(llamaDeployment("x", "ns", 1).Spec)
	s.Rollout.Steps = []int32{50, 20}
	if _, err := planFor(s); err == nil {
		t.Error("expected invalid steps to be rejected")
	}
}

func TestSamplesPlan(t *testing.T) {
	tests := map[string]struct {
		quant   servingv1alpha1.Quantization
		profile string
	}{
		"serving_v1alpha1_modeldeployment_llama.yaml": {servingv1alpha1.QuantizationAWQ, "1g.20gb"},
		"serving_v1alpha1_modeldeployment_qwen.yaml":  {servingv1alpha1.QuantizationAWQ, "1g.20gb"},
	}
	for file, want := range tests {
		data, err := os.ReadFile("../../config/samples/" + file)
		if err != nil {
			t.Fatal(err)
		}
		md := &servingv1alpha1.ModelDeployment{}
		if err := yaml.UnmarshalStrict(data, md); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		p, err := planFor(withDefaults(md.Spec))
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if p.Variant.Quantization != planner.Quantization(want.quant) || p.Profile.Name != want.profile {
			t.Errorf("%s: plan = %s on %s, want %s on %s", file, p.Variant, p.Profile.Name, want.quant, want.profile)
		}
	}
}
