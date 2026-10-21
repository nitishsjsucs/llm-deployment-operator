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

package planner

import (
	"errors"
	"math"
	"testing"

	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
)

var (
	fp16  = Variant{Quantization: FP16, Bits: 16, URI: "hf://org/model"}
	awq   = Variant{Quantization: AWQ, Bits: 4, URI: "hf://org/model-awq"}
	gptq4 = Variant{Quantization: GPTQ, Bits: 4, URI: "hf://org/model-gptq-int4"}
	gptq8 = Variant{Quantization: GPTQ, Bits: 8, URI: "hf://org/model-gptq-int8"}
)

func arch(t *testing.T, id string) Architecture {
	t.Helper()
	a, ok := Lookup(id)
	if !ok {
		t.Fatalf("catalog entry %s missing", id)
	}
	return a
}

func gpu(t *testing.T, name string) packing.GPUModel {
	t.Helper()
	g, ok := packing.LookupGPU(name)
	if !ok {
		t.Fatalf("GPU %s missing", name)
	}
	return g
}

func TestCatalogEntriesAreValid(t *testing.T) {
	for _, id := range CatalogIDs() {
		a := arch(t, id)
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		if a.Family != "llama" && a.Family != "qwen" {
			t.Errorf("%s: unexpected family %q", id, a.Family)
		}
	}
	if len(CatalogIDs()) != 5 {
		t.Errorf("catalog has %d entries, want 5", len(CatalogIDs()))
	}
}

func TestKVCacheBytesPerToken(t *testing.T) {
	tests := map[string]int64{
		"llama-3.1-8b":  2 * 32 * 8 * 128 * 2, // 128 KiB
		"llama-3.1-70b": 2 * 80 * 8 * 128 * 2, // 320 KiB
		"qwen2.5-7b":    2 * 28 * 4 * 128 * 2, // 56 KiB
		"qwen2.5-14b":   2 * 48 * 8 * 128 * 2, // 192 KiB
		"qwen2.5-72b":   2 * 80 * 8 * 128 * 2, // 320 KiB
	}
	for id, want := range tests {
		if got := KVCacheBytesPerToken(arch(t, id)); got != want {
			t.Errorf("%s: %d bytes/token, want %d", id, got, want)
		}
	}
}

func TestWeightBytes(t *testing.T) {
	llama8b := arch(t, "llama-3.1-8b")
	embed := int64(128_256 * 4096 * 2)
	quantized := llama8b.Params - embed
	tests := []struct {
		name    string
		variant Variant
		want    int64
	}{
		{"fp16 is 2 bytes per parameter", fp16, 8_030_261_248 * 2},
		// 4 bits + (16-bit scale + 4-bit zero) / 128 = 4.15625 bits per weight.
		{"awq int4", awq, int64(math.Ceil(float64(quantized)*4.15625/8)) + embed*2},
		{"gptq int4 matches awq int4", gptq4, int64(math.Ceil(float64(quantized)*4.15625/8)) + embed*2},
		// 8 bits + (16 + 8) / 128 = 8.1875 bits per weight.
		{"gptq int8", gptq8, int64(math.Ceil(float64(quantized)*8.1875/8)) + embed*2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WeightBytes(llama8b, tt.variant); got != tt.want {
				t.Errorf("WeightBytes = %d, want %d", got, tt.want)
			}
		})
	}

	// Tied embeddings are counted once.
	tied := llama8b
	tied.TiedEmbeddings = true
	if got, want := tied.EmbeddingParams(), int64(128_256*4096); got != want {
		t.Errorf("tied EmbeddingParams = %d, want %d", got, want)
	}
}

func TestEstimateMemoryLlama8BAWQ(t *testing.T) {
	e := EstimateMemory(arch(t, "llama-3.1-8b"), awq, Workload{MaxModelLen: 8192, MaxNumSeqs: 8})
	if e.KVCacheBytes != 8*GiB {
		t.Errorf("KV cache = %d bytes, want exactly 8 GiB (128 KiB x 65536 tokens)", e.KVCacheBytes)
	}
	if w := float64(e.WeightsBytes) / GiB; w < 5.3 || w > 5.4 {
		t.Errorf("weights = %.3f GiB, want about 5.33 GiB", w)
	}
	if got := e.TotalGiB(); got < 14.8 || got > 14.9 {
		t.Errorf("total = %.3f GiB, want about 14.83 GiB", got)
	}
}

func TestChoose(t *testing.T) {
	w8x8 := Workload{MaxModelLen: 8192, MaxNumSeqs: 8}
	tests := []struct {
		name        string
		arch        string
		gpu         string
		variants    []Variant
		budgetGiB   float64
		workload    Workload
		wantVariant Variant
		wantProfile string
		wantPerGPU  int
		wantErr     error
		wantAnyErr  bool
	}{
		{
			name: "4-bit 8B fits 1g.20gb, four replicas per A100-80GB",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{fp16, awq, gptq4}, budgetGiB: 20, workload: w8x8,
			wantVariant: awq, wantProfile: "1g.20gb", wantPerGPU: 4,
		},
		{
			name: "without a budget the fp16 variant wins on 3g.40gb",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{fp16, awq, gptq4}, workload: w8x8,
			wantVariant: fp16, wantProfile: "3g.40gb", wantPerGPU: 2,
		},
		{
			name: "AWQ wins the int4 tie-break regardless of order",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{gptq4, awq}, budgetGiB: 20, workload: w8x8,
			wantVariant: awq, wantProfile: "1g.20gb", wantPerGPU: 4,
		},
		{
			name: "GPTQ is used when it is the only int4 variant",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{fp16, gptq4}, budgetGiB: 20, workload: w8x8,
			wantVariant: gptq4, wantProfile: "1g.20gb", wantPerGPU: 4,
		},
		{
			name: "int8 GPTQ does not fit 20 GiB with 8x8192 KV, so int4 wins",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{gptq8, awq}, budgetGiB: 20, workload: w8x8,
			wantVariant: awq, wantProfile: "1g.20gb", wantPerGPU: 4,
		},
		{
			name: "int8 GPTQ is preferred over int4 when it fits",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{awq, gptq8}, budgetGiB: 40, workload: w8x8,
			wantVariant: gptq8, wantProfile: "3g.40gb", wantPerGPU: 2,
		},
		{
			name: "more concurrency outgrows 1g.20gb",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{awq}, workload: Workload{MaxModelLen: 8192, MaxNumSeqs: 16},
			wantVariant: awq, wantProfile: "3g.40gb", wantPerGPU: 2,
		},
		{
			name: "a small KV cache fits 1g.10gb",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{awq}, workload: Workload{MaxModelLen: 2048, MaxNumSeqs: 4},
			wantVariant: awq, wantProfile: "1g.10gb", wantPerGPU: 7,
		},
		{
			name: "Qwen2.5-7B AWQ on 1g.20gb",
			arch: "qwen2.5-7b", gpu: "A100-80GB", variants: []Variant{fp16, awq}, budgetGiB: 20, workload: w8x8,
			wantVariant: awq, wantProfile: "1g.20gb", wantPerGPU: 4,
		},
		{
			name: "Qwen2.5-14B GPTQ int4 needs 3g.40gb at 8x8192",
			arch: "qwen2.5-14b", gpu: "A100-80GB", variants: []Variant{fp16, gptq4}, budgetGiB: 40, workload: w8x8,
			wantVariant: gptq4, wantProfile: "3g.40gb", wantPerGPU: 2,
		},
		{
			name: "Llama-3.1-70B AWQ takes a whole A100-80GB",
			arch: "llama-3.1-70b", gpu: "A100-80GB", variants: []Variant{awq}, workload: w8x8,
			wantVariant: awq, wantProfile: "7g.80gb", wantPerGPU: 1,
		},
		{
			name: "Qwen2.5-72B GPTQ int4 on H100-80GB",
			arch: "qwen2.5-72b", gpu: "H100-80GB", variants: []Variant{gptq4}, workload: w8x8,
			wantVariant: gptq4, wantProfile: "7g.80gb", wantPerGPU: 1,
		},
		{
			name: "8B AWQ on A100-40GB uses 3g.20gb",
			arch: "llama-3.1-8b", gpu: "A100-40GB", variants: []Variant{awq}, workload: w8x8,
			wantVariant: awq, wantProfile: "3g.20gb", wantPerGPU: 2,
		},
		{
			name: "fp16 70B fits no single MIG instance",
			arch: "llama-3.1-70b", gpu: "A100-80GB", variants: []Variant{fp16}, workload: w8x8,
			wantErr: ErrNoFit,
		},
		{
			name: "budget below every profile",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{awq}, budgetGiB: 5, workload: w8x8,
			wantErr: ErrNoFit,
		},
		{
			name: "maxModelLen beyond the context window",
			arch: "qwen2.5-7b", gpu: "A100-80GB", variants: []Variant{awq}, workload: Workload{MaxModelLen: 65536, MaxNumSeqs: 1},
			wantAnyErr: true,
		},
		{
			name: "AWQ with 8 bits is invalid",
			arch: "llama-3.1-8b", gpu: "A100-80GB", variants: []Variant{{Quantization: AWQ, Bits: 8}}, workload: w8x8,
			wantAnyErr: true,
		},
		{
			name: "no variants", arch: "llama-3.1-8b", gpu: "A100-80GB", workload: w8x8,
			wantAnyErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Choose(Request{
				Arch:                   arch(t, tt.arch),
				Variants:               tt.variants,
				GPU:                    gpu(t, tt.gpu),
				MaxMemoryGiBPerReplica: tt.budgetGiB,
				Workload:               tt.workload,
			})
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case tt.wantAnyErr:
				if err == nil {
					t.Fatalf("expected an error, got plan %+v", plan)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			if plan.Variant != tt.wantVariant {
				t.Errorf("variant = %s, want %s", plan.Variant, tt.wantVariant)
			}
			if plan.Profile.Name != tt.wantProfile {
				t.Errorf("profile = %s (estimate %.2f GiB), want %s", plan.Profile.Name, plan.Estimate.TotalGiB(), tt.wantProfile)
			}
			if plan.ReplicasPerGPU != tt.wantPerGPU {
				t.Errorf("replicas per GPU = %d, want %d", plan.ReplicasPerGPU, tt.wantPerGPU)
			}
			if !Fits(plan.Estimate, plan.Profile) {
				t.Errorf("chosen profile does not fit the estimate")
			}
		})
	}
}

func TestChoosePicksSmallestFittingProfile(t *testing.T) {
	g := gpu(t, "A100-80GB")
	a := arch(t, "llama-3.1-8b")
	w := Workload{MaxModelLen: 8192, MaxNumSeqs: 8}
	plan, err := Choose(Request{Arch: a, Variants: []Variant{awq}, GPU: g, Workload: w})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range g.Profiles {
		if p.MemoryGiB < plan.Profile.MemoryGiB && Fits(plan.Estimate, p) {
			t.Errorf("smaller profile %s also fits", p.Name)
		}
		// 2g.20gb has the same memory as 1g.20gb but more compute slices.
		if p.MemoryGiB == plan.Profile.MemoryGiB && p.ComputeSlices < plan.Profile.ComputeSlices {
			t.Errorf("profile %s uses fewer compute slices", p.Name)
		}
	}
}
