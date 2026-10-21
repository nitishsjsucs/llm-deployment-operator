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

// Package packing models NVIDIA Multi-Instance GPU (MIG) profiles and packs
// replica slice requests onto physical GPUs.
package packing

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
)

// Profile is a MIG GPU-instance profile such as 1g.20gb.
type Profile struct {
	// Name is the profile name as reported by nvidia-smi and the NVIDIA DRA driver.
	Name string
	// ComputeSlices is the number of compute (SM) slices the instance uses.
	ComputeSlices int
	// MemorySlices is the number of memory slices the instance uses.
	MemorySlices int
	// MemoryGiB is the usable framebuffer memory of the instance as reported by
	// `nvidia-smi mig -lgip`. It is slightly below the nominal size because the
	// driver reserves some memory per instance; values vary a little by driver.
	MemoryGiB float64
	// MaxInstances is the maximum number of instances of this profile per GPU.
	MaxInstances int
	// Placements are the memory-slice offsets at which an instance may start
	// (the "Placements" column of `nvidia-smi mig -lgipp`).
	Placements []int
}

// GPUModel describes a MIG-capable GPU and its instance profiles.
type GPUModel struct {
	// Name is the short GPU type used in the API, e.g. A100-80GB.
	Name string
	// ProductNamePattern is a regular expression matched against the
	// productName attribute published by the NVIDIA DRA driver.
	ProductNamePattern string
	// ComputeSlices is the number of compute slices of the whole GPU.
	ComputeSlices int
	// MemorySlices is the number of memory slices of the whole GPU.
	MemorySlices int
	// MemoryGiB is the total framebuffer memory of the GPU.
	MemoryGiB float64
	// Profiles are ordered from smallest to largest.
	Profiles []Profile
}

// The tables below cover the compute-only profiles (the "+me" media-extension
// variants are omitted). Instance counts and placements follow the NVIDIA MIG
// user guide; the GPU has 7 compute slices and 8 memory slices, and the
// eighth memory slice is only reachable by profiles that span it.
var gpuModels = map[string]GPUModel{
	"A100-80GB": {
		Name:               "A100-80GB",
		ProductNamePattern: "A100.*80GB",
		ComputeSlices:      7,
		MemorySlices:       8,
		MemoryGiB:          80,
		Profiles: []Profile{
			{Name: "1g.10gb", ComputeSlices: 1, MemorySlices: 1, MemoryGiB: 9.5, MaxInstances: 7, Placements: []int{0, 1, 2, 3, 4, 5, 6}},
			{Name: "1g.20gb", ComputeSlices: 1, MemorySlices: 2, MemoryGiB: 19.5, MaxInstances: 4, Placements: []int{0, 2, 4, 6}},
			{Name: "2g.20gb", ComputeSlices: 2, MemorySlices: 2, MemoryGiB: 19.5, MaxInstances: 3, Placements: []int{0, 2, 4}},
			{Name: "3g.40gb", ComputeSlices: 3, MemorySlices: 4, MemoryGiB: 39.25, MaxInstances: 2, Placements: []int{0, 4}},
			{Name: "4g.40gb", ComputeSlices: 4, MemorySlices: 4, MemoryGiB: 39.25, MaxInstances: 1, Placements: []int{0}},
			{Name: "7g.80gb", ComputeSlices: 7, MemorySlices: 8, MemoryGiB: 78.75, MaxInstances: 1, Placements: []int{0}},
		},
	},
	"A100-40GB": {
		Name:               "A100-40GB",
		ProductNamePattern: "A100.*40GB",
		ComputeSlices:      7,
		MemorySlices:       8,
		MemoryGiB:          40,
		Profiles: []Profile{
			{Name: "1g.5gb", ComputeSlices: 1, MemorySlices: 1, MemoryGiB: 4.75, MaxInstances: 7, Placements: []int{0, 1, 2, 3, 4, 5, 6}},
			{Name: "1g.10gb", ComputeSlices: 1, MemorySlices: 2, MemoryGiB: 9.62, MaxInstances: 4, Placements: []int{0, 2, 4, 6}},
			{Name: "2g.10gb", ComputeSlices: 2, MemorySlices: 2, MemoryGiB: 9.62, MaxInstances: 3, Placements: []int{0, 2, 4}},
			{Name: "3g.20gb", ComputeSlices: 3, MemorySlices: 4, MemoryGiB: 19.62, MaxInstances: 2, Placements: []int{0, 4}},
			{Name: "4g.20gb", ComputeSlices: 4, MemorySlices: 4, MemoryGiB: 19.62, MaxInstances: 1, Placements: []int{0}},
			{Name: "7g.40gb", ComputeSlices: 7, MemorySlices: 8, MemoryGiB: 39.5, MaxInstances: 1, Placements: []int{0}},
		},
	},
	"H100-80GB": {
		Name:               "H100-80GB",
		ProductNamePattern: "H100",
		ComputeSlices:      7,
		MemorySlices:       8,
		MemoryGiB:          80,
		Profiles: []Profile{
			{Name: "1g.10gb", ComputeSlices: 1, MemorySlices: 1, MemoryGiB: 9.75, MaxInstances: 7, Placements: []int{0, 1, 2, 3, 4, 5, 6}},
			{Name: "1g.20gb", ComputeSlices: 1, MemorySlices: 2, MemoryGiB: 19.62, MaxInstances: 4, Placements: []int{0, 2, 4, 6}},
			{Name: "2g.20gb", ComputeSlices: 2, MemorySlices: 2, MemoryGiB: 19.62, MaxInstances: 3, Placements: []int{0, 2, 4}},
			{Name: "3g.40gb", ComputeSlices: 3, MemorySlices: 4, MemoryGiB: 39.5, MaxInstances: 2, Placements: []int{0, 4}},
			{Name: "4g.40gb", ComputeSlices: 4, MemorySlices: 4, MemoryGiB: 39.5, MaxInstances: 1, Placements: []int{0}},
			{Name: "7g.80gb", ComputeSlices: 7, MemorySlices: 8, MemoryGiB: 79.25, MaxInstances: 1, Placements: []int{0}},
		},
	},
}

// LookupGPU returns the GPU model with the given name.
func LookupGPU(name string) (GPUModel, bool) {
	m, ok := gpuModels[name]
	return m, ok
}

// GPUNames returns the supported GPU types in sorted order.
func GPUNames() []string {
	names := make([]string, 0, len(gpuModels))
	for n := range gpuModels {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Profile returns the named profile of the GPU model.
func (g GPUModel) Profile(name string) (Profile, bool) {
	for _, p := range g.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// InstancesPerGPU is how many instances of the profile fit on one GPU when the
// GPU is dedicated to that profile, bounded by the instance limit and by the
// compute and memory slices.
func (g GPUModel) InstancesPerGPU(p Profile) int {
	return min(p.MaxInstances, g.ComputeSlices/p.ComputeSlices, g.MemorySlices/p.MemorySlices)
}

var profileNameRE = regexp.MustCompile(`^([1-7])g\.([0-9]+)gb$`)

// NominalMemoryGiB parses the nominal memory size from a profile name, e.g. 20
// for 1g.20gb. On A100 and H100 one memory slice is 1/8 of the framebuffer, so
// the nominal number is the memory the instance takes away from the GPU.
func NominalMemoryGiB(profile string) (int64, error) {
	m := profileNameRE.FindStringSubmatch(profile)
	if m == nil {
		return 0, fmt.Errorf("invalid MIG profile name %q", profile)
	}
	return strconv.ParseInt(m[2], 10, 64)
}
