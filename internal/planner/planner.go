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
	"fmt"
	"sort"
	"strings"

	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
)

// GPUMemoryUtilization is the fraction of a MIG instance's memory a replica
// may use. It is passed to vLLM as --gpu-memory-utilization, and a plan only
// fits a profile when the estimate is within this fraction of its memory.
const GPUMemoryUtilization = 0.90

// Request is the input to Choose.
type Request struct {
	Arch     Architecture
	Variants []Variant
	GPU      packing.GPUModel
	// MaxMemoryGiBPerReplica caps the usable memory of profiles that may be
	// chosen. Zero means no cap.
	MaxMemoryGiBPerReplica float64
	Workload               Workload
}

// Plan is the chosen variant and MIG profile for a deployment.
type Plan struct {
	Variant  Variant
	Profile  packing.Profile
	Estimate Estimate
	// ReplicasPerGPU is how many replicas of this plan one GPU can host.
	ReplicasPerGPU int
}

// ErrNoFit is wrapped by Choose when no variant fits any allowed profile.
var ErrNoFit = errors.New("no variant fits the GPU budget")

// qualityRank orders variants by expected output quality: more weight bits
// first. Variants with the same rank are tie-broken by Choose.
func qualityRank(v Variant) int64 { return v.Bits }

// methodOrder is the final tie-break between AWQ and GPTQ at equal bits and
// equal estimated memory. AWQ comes first; the preference is a fixed
// convention so that plans are stable across reconciles, not a quality claim.
var methodOrder = map[Quantization]int{FP16: 0, AWQ: 1, GPTQ: 2}

type candidate struct {
	index    int
	variant  Variant
	estimate Estimate
}

// Choose returns the highest-quality variant that fits the budget, placed on
// the smallest MIG profile that holds it.
//
// Variants are tried in this order:
//  1. more weight bits first (fp16 > int8 > int4);
//  2. then the smaller memory estimate;
//  3. then AWQ before GPTQ;
//  4. then the order in which they are listed.
//
// Profiles are tried from least to most usable memory, then fewest compute
// slices, skipping those above MaxMemoryGiBPerReplica. A variant fits a
// profile when its estimate is at most GPUMemoryUtilization of the profile's
// usable memory.
func Choose(req Request) (Plan, error) {
	if len(req.Variants) == 0 {
		return Plan{}, fmt.Errorf("at least one variant is required")
	}
	if err := req.Arch.Validate(); err != nil {
		return Plan{}, err
	}
	if req.Workload.MaxModelLen <= 0 || req.Workload.MaxNumSeqs <= 0 {
		return Plan{}, fmt.Errorf("maxModelLen and maxConcurrentSequences must be positive")
	}
	if req.Arch.MaxContextLen > 0 && req.Workload.MaxModelLen > req.Arch.MaxContextLen {
		return Plan{}, fmt.Errorf("maxModelLen %d exceeds the model's context window of %d", req.Workload.MaxModelLen, req.Arch.MaxContextLen)
	}

	cands := make([]candidate, 0, len(req.Variants))
	for i, v := range req.Variants {
		if err := v.Validate(); err != nil {
			return Plan{}, fmt.Errorf("variant %d: %w", i, err)
		}
		cands = append(cands, candidate{index: i, variant: v, estimate: EstimateMemory(req.Arch, v, req.Workload)})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if qa, qb := qualityRank(a.variant), qualityRank(b.variant); qa != qb {
			return qa > qb
		}
		if ta, tb := a.estimate.TotalBytes(), b.estimate.TotalBytes(); ta != tb {
			return ta < tb
		}
		if ma, mb := methodOrder[a.variant.Quantization], methodOrder[b.variant.Quantization]; ma != mb {
			return ma < mb
		}
		return a.index < b.index
	})

	profiles := allowedProfiles(req.GPU, req.MaxMemoryGiBPerReplica)
	if len(profiles) == 0 {
		return Plan{}, fmt.Errorf("%w: no %s MIG profile has at most %.2f GiB", ErrNoFit, req.GPU.Name, req.MaxMemoryGiBPerReplica)
	}
	for _, c := range cands {
		for _, p := range profiles {
			if Fits(c.estimate, p) {
				return Plan{
					Variant:        c.variant,
					Profile:        p,
					Estimate:       c.estimate,
					ReplicasPerGPU: req.GPU.InstancesPerGPU(p),
				}, nil
			}
		}
	}

	largest := profiles[len(profiles)-1]
	var needs []string
	for _, c := range cands {
		needs = append(needs, fmt.Sprintf("%s needs %.2f GiB", c.variant, c.estimate.TotalGiB()))
	}
	return Plan{}, fmt.Errorf("%w: largest allowed %s profile %s offers %.2f GiB at %.0f%% utilization; %s",
		ErrNoFit, req.GPU.Name, largest.Name, UsableGiB(largest), GPUMemoryUtilization*100, strings.Join(needs, ", "))
}

// UsableGiB is the memory of a profile that a replica may plan to use.
func UsableGiB(p packing.Profile) float64 { return p.MemoryGiB * GPUMemoryUtilization }

// Fits reports whether the estimate fits in the profile.
func Fits(e Estimate, p packing.Profile) bool {
	return float64(e.TotalBytes()) <= UsableGiB(p)*GiB
}

func allowedProfiles(gpu packing.GPUModel, maxGiB float64) []packing.Profile {
	var out []packing.Profile
	for _, p := range gpu.Profiles {
		if maxGiB > 0 && p.MemoryGiB > maxGiB {
			continue
		}
		out = append(out, p)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].MemoryGiB != out[j].MemoryGiB {
			return out[i].MemoryGiB < out[j].MemoryGiB
		}
		return out[i].ComputeSlices < out[j].ComputeSlices
	})
	return out
}
