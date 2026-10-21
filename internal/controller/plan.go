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
	"fmt"
	"strconv"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/planner"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/rollout"
)

// withDefaults fills in the CRD schema defaults. The API server applies them
// on admission; doing it here as well keeps the controller correct for
// objects stored before a default was introduced.
func withDefaults(in servingv1alpha1.ModelDeploymentSpec) servingv1alpha1.ModelDeploymentSpec {
	s := *in.DeepCopy()
	if s.Replicas == nil {
		s.Replicas = ptr.To(servingv1alpha1.DefaultReplicas)
	}
	if s.GPUBudget.MaxModelLen == 0 {
		s.GPUBudget.MaxModelLen = servingv1alpha1.DefaultMaxModelLen
	}
	if s.GPUBudget.MaxConcurrentSequences == 0 {
		s.GPUBudget.MaxConcurrentSequences = servingv1alpha1.DefaultMaxConcurrentSequences
	}
	if s.Rollout.Strategy == "" {
		s.Rollout.Strategy = servingv1alpha1.RolloutStrategyCanary
	}
	if len(s.Rollout.Steps) == 0 {
		s.Rollout.Steps = append([]int32(nil), servingv1alpha1.DefaultCanarySteps...)
	}
	if s.Rollout.StepInterval == nil {
		s.Rollout.StepInterval = &metav1.Duration{Duration: servingv1alpha1.DefaultStepInterval}
	}
	if s.Rollout.ProgressDeadline == nil {
		s.Rollout.ProgressDeadline = &metav1.Duration{Duration: servingv1alpha1.DefaultProgressDeadline}
	}
	if s.Routing.Gateway.Name == "" {
		s.Routing.Gateway.Name = servingv1alpha1.DefaultGatewayName
	}
	if s.Routing.Gateway.Namespace == "" {
		s.Routing.Gateway.Namespace = servingv1alpha1.DefaultGatewayNamespace
	}
	return s
}

func rolloutConfig(s servingv1alpha1.ModelDeploymentSpec) rollout.Config {
	return rollout.Config{
		Strategy:         rollout.Strategy(s.Rollout.Strategy),
		Steps:            s.Rollout.Steps,
		StepInterval:     s.Rollout.StepInterval.Duration,
		ProgressDeadline: s.Rollout.ProgressDeadline.Duration,
	}
}

func architectureFor(m servingv1alpha1.ModelSpec) (planner.Architecture, error) {
	if m.CatalogRef != "" {
		a, ok := planner.Lookup(m.CatalogRef)
		if !ok {
			return planner.Architecture{}, fmt.Errorf("unknown catalog entry %q", m.CatalogRef)
		}
		if a.Family != string(m.Family) {
			return planner.Architecture{}, fmt.Errorf("catalog entry %q is a %s model, not %s", m.CatalogRef, a.Family, m.Family)
		}
		return a, nil
	}
	if m.Architecture == nil {
		return planner.Architecture{}, fmt.Errorf("one of model.catalogRef or model.architecture is required")
	}
	a := m.Architecture
	return planner.Architecture{
		Family:         string(m.Family),
		Params:         a.ParametersMillions * 1_000_000,
		Layers:         int64(a.Layers),
		KVHeads:        int64(a.KVHeads),
		HeadDim:        int64(a.HeadDim),
		HiddenSize:     int64(a.HiddenSize),
		VocabSize:      int64(a.VocabSize),
		TiedEmbeddings: a.TiedEmbeddings,
		MaxContextLen:  int64(a.MaxContextLength),
	}, nil
}

func variantBits(v servingv1alpha1.ModelVariant) int32 {
	switch {
	case v.Bits != 0:
		return v.Bits
	case v.Quantization == servingv1alpha1.QuantizationFP16:
		return 16
	default:
		return 4
	}
}

// planFor runs the planner for a defaulted spec.
func planFor(s servingv1alpha1.ModelDeploymentSpec) (planner.Plan, error) {
	if err := rollout.ValidateSteps(s.Rollout.Steps); err != nil {
		return planner.Plan{}, fmt.Errorf("rollout.steps: %w", err)
	}
	arch, err := architectureFor(s.Model)
	if err != nil {
		return planner.Plan{}, err
	}
	gpu, ok := packing.LookupGPU(s.GPUBudget.GPUType)
	if !ok {
		return planner.Plan{}, fmt.Errorf("unsupported GPU type %q", s.GPUBudget.GPUType)
	}
	variants := make([]planner.Variant, 0, len(s.Variants))
	for _, v := range s.Variants {
		variants = append(variants, planner.Variant{
			Quantization: planner.Quantization(v.Quantization),
			Bits:         int64(variantBits(v)),
			URI:          v.URI,
		})
	}
	var maxGiB float64
	if s.GPUBudget.MaxMemoryGiBPerReplica != nil {
		maxGiB = float64(*s.GPUBudget.MaxMemoryGiBPerReplica)
	}
	return planner.Choose(planner.Request{
		Arch:                   arch,
		Variants:               variants,
		GPU:                    gpu,
		MaxMemoryGiBPerReplica: maxGiB,
		Workload: planner.Workload{
			MaxModelLen: int64(s.GPUBudget.MaxModelLen),
			MaxNumSeqs:  int64(s.GPUBudget.MaxConcurrentSequences),
		},
	})
}

// templateFor is the revision template of a defaulted spec and its plan.
func templateFor(s servingv1alpha1.ModelDeploymentSpec, p planner.Plan) render.Template {
	return render.Template{
		ModelName: s.Model.Name,
		Variant: servingv1alpha1.SelectedVariant{
			Quantization: servingv1alpha1.Quantization(p.Variant.Quantization),
			Bits:         int32(p.Variant.Bits),
			URI:          p.Variant.URI,
		},
		GPUType:              s.GPUBudget.GPUType,
		MIGProfile:           p.Profile.Name,
		MaxModelLen:          s.GPUBudget.MaxModelLen,
		MaxNumSeqs:           s.GPUBudget.MaxConcurrentSequences,
		GPUMemoryUtilization: strconv.FormatFloat(planner.GPUMemoryUtilization, 'f', 2, 64),
		Image:                s.Runtime.Image,
		ExtraArgs:            s.Runtime.ExtraArgs,
		Env:                  s.Runtime.Env,
	}
}

func mib(bytes int64) int64 { return bytes >> 20 }

// planSummary is the message of the Planned condition.
func planSummary(p planner.Plan, gpuType string) string {
	return fmt.Sprintf("%s on %s: estimated %.2f GiB (weights %.2f, KV cache %.2f, overhead %.2f) of %.2f GiB usable; %d replicas per %s",
		p.Variant, p.Profile.Name, p.Estimate.TotalGiB(),
		float64(p.Estimate.WeightsBytes)/planner.GiB, float64(p.Estimate.KVCacheBytes)/planner.GiB, float64(p.Estimate.OverheadBytes)/planner.GiB,
		planner.UsableGiB(p.Profile), p.ReplicasPerGPU, gpuType)
}

func memoryEstimate(p planner.Plan) *servingv1alpha1.MemoryEstimate {
	return &servingv1alpha1.MemoryEstimate{
		WeightsMiB:  mib(p.Estimate.WeightsBytes),
		KVCacheMiB:  mib(p.Estimate.KVCacheBytes),
		OverheadMiB: mib(p.Estimate.OverheadBytes),
		TotalMiB:    mib(p.Estimate.TotalBytes()),
		ProfileMiB:  int64(p.Profile.MemoryGiB * 1024),
	}
}
