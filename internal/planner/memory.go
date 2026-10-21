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

// Package planner estimates the GPU memory a vLLM replica needs and picks the
// model variant and MIG profile for a deployment.
package planner

import "fmt"

// Quantization is the weight format of a model variant.
type Quantization string

const (
	FP16 Quantization = "fp16"
	AWQ  Quantization = "awq"
	GPTQ Quantization = "gptq"
)

const (
	// GiB is 2^30 bytes.
	GiB = 1 << 30

	// QuantGroupSize is the quantization group size assumed for AWQ and GPTQ
	// checkpoints. Every group of weights stores a 16-bit scale and a packed
	// zero point of the same width as the weights.
	QuantGroupSize = 128

	// KVCacheBytesPerElement is the size of one cached key or value element
	// (the fp16/bf16 KV cache that vLLM uses by default).
	KVCacheBytesPerElement = 2

	// RuntimeOverheadBytes covers the CUDA context, the activation workspace
	// vLLM profiles at startup, and CUDA graph capture. It is a fixed
	// allowance, not a measurement; calibrate it for your vLLM version.
	RuntimeOverheadBytes = 3 * GiB / 2
)

// Variant is one checkpoint of a model.
type Variant struct {
	Quantization Quantization
	// Bits is the weight width: 16 for fp16, 4 for AWQ, 4 or 8 for GPTQ.
	Bits int64
	URI  string
}

// Validate checks that the bit width is valid for the quantization method.
func (v Variant) Validate() error {
	switch v.Quantization {
	case FP16:
		if v.Bits == 16 {
			return nil
		}
	case AWQ:
		if v.Bits == 4 {
			return nil
		}
	case GPTQ:
		if v.Bits == 4 || v.Bits == 8 {
			return nil
		}
	default:
		return fmt.Errorf("unknown quantization %q", v.Quantization)
	}
	return fmt.Errorf("%s does not support %d-bit weights", v.Quantization, v.Bits)
}

// String renders the variant as e.g. "awq-int4".
func (v Variant) String() string {
	if v.Quantization == FP16 {
		return string(FP16)
	}
	return fmt.Sprintf("%s-int%d", v.Quantization, v.Bits)
}

// Workload is the serving shape a replica must sustain without preempting
// sequences.
type Workload struct {
	// MaxModelLen is the context length per sequence (vLLM --max-model-len).
	MaxModelLen int64
	// MaxNumSeqs is the number of concurrent sequences (vLLM --max-num-seqs).
	MaxNumSeqs int64
}

// Estimate is the memory a replica needs, by component.
type Estimate struct {
	WeightsBytes  int64
	KVCacheBytes  int64
	OverheadBytes int64
}

// TotalBytes is the sum of all components.
func (e Estimate) TotalBytes() int64 {
	return e.WeightsBytes + e.KVCacheBytes + e.OverheadBytes
}

// TotalGiB is TotalBytes in GiB.
func (e Estimate) TotalGiB() float64 { return float64(e.TotalBytes()) / GiB }

// WeightBytes is the size of the variant's weights. Quantized checkpoints keep
// the embedding and LM head in 16-bit; every other weight costs
// bits + (16 + bits)/QuantGroupSize bits (the weight plus its share of the
// group's scale and zero point).
func WeightBytes(a Architecture, v Variant) int64 {
	if v.Quantization == FP16 {
		return a.Params * 2
	}
	quantized := a.Params - a.EmbeddingParams()
	bitsTimesGroup := v.Bits*QuantGroupSize + 16 + v.Bits
	quantizedBytes := ceilDiv(quantized*bitsTimesGroup, 8*QuantGroupSize)
	return quantizedBytes + a.EmbeddingParams()*2
}

// KVCacheBytesPerToken is 2 (K and V) x layers x kv heads x head dim x element size.
func KVCacheBytesPerToken(a Architecture) int64 {
	return 2 * a.Layers * a.KVHeads * a.HeadDim * KVCacheBytesPerElement
}

// EstimateMemory sizes a replica for the worst case of MaxNumSeqs sequences
// all at MaxModelLen tokens, so the KV cache never has to preempt.
func EstimateMemory(a Architecture, v Variant, w Workload) Estimate {
	return Estimate{
		WeightsBytes:  WeightBytes(a, v),
		KVCacheBytes:  KVCacheBytesPerToken(a) * w.MaxModelLen * w.MaxNumSeqs,
		OverheadBytes: RuntimeOverheadBytes,
	}
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }
