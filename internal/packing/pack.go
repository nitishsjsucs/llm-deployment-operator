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

package packing

import (
	"fmt"
	"sort"
)

// Request asks for one MIG instance, typically for one model replica.
type Request struct {
	// ID identifies the requester in the result, e.g. "llama-8b/0".
	ID string
	// Profile is the MIG profile name.
	Profile string
}

// Instance is a MIG instance placed on a GPU.
type Instance struct {
	RequestID string
	Profile   Profile
	// Start is the first memory slice the instance occupies.
	Start int
}

// GPU is one physical GPU and the instances packed onto it.
type GPU struct {
	Instances []Instance
	// memUsed marks occupied memory slices.
	memUsed       []bool
	computeUsed   int
	profileCounts map[string]int
}

// ComputeSlicesUsed returns the number of compute slices in use.
func (g *GPU) ComputeSlicesUsed() int { return g.computeUsed }

// MemorySlicesUsed returns the number of memory slices in use.
func (g *GPU) MemorySlicesUsed() int {
	n := 0
	for _, used := range g.memUsed {
		if used {
			n++
		}
	}
	return n
}

// AllocatedMemoryGiB returns the usable memory of all instances on the GPU.
func (g *GPU) AllocatedMemoryGiB() float64 {
	total := 0.0
	for _, in := range g.Instances {
		total += in.Profile.MemoryGiB
	}
	return total
}

func newGPU(model GPUModel) *GPU {
	return &GPU{memUsed: make([]bool, model.MemorySlices), profileCounts: map[string]int{}}
}

// place puts p on the GPU at the lowest valid offset and reports whether it fit.
func (g *GPU) place(model GPUModel, p Profile, id string) bool {
	if g.profileCounts[p.Name] >= p.MaxInstances || g.computeUsed+p.ComputeSlices > model.ComputeSlices {
		return false
	}
	for _, start := range p.Placements {
		if start+p.MemorySlices > model.MemorySlices || !g.free(start, p.MemorySlices) {
			continue
		}
		for i := start; i < start+p.MemorySlices; i++ {
			g.memUsed[i] = true
		}
		g.computeUsed += p.ComputeSlices
		g.profileCounts[p.Name]++
		g.Instances = append(g.Instances, Instance{RequestID: id, Profile: p, Start: start})
		return true
	}
	return false
}

func (g *GPU) free(start, n int) bool {
	for i := start; i < start+n; i++ {
		if g.memUsed[i] {
			return false
		}
	}
	return true
}

// Pack places every request onto as few GPUs as first-fit-decreasing finds.
// Requests are sorted by memory slices, then compute slices (both descending),
// then by ID so that the result is deterministic. Each request goes to the
// first GPU with a free valid placement; a new GPU is opened otherwise.
func Pack(model GPUModel, reqs []Request) ([]*GPU, error) {
	type item struct {
		req     Request
		profile Profile
	}
	items := make([]item, 0, len(reqs))
	for _, r := range reqs {
		p, ok := model.Profile(r.Profile)
		if !ok {
			return nil, fmt.Errorf("request %q: profile %q is not available on %s", r.ID, r.Profile, model.Name)
		}
		items = append(items, item{req: r, profile: p})
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i].profile, items[j].profile
		if a.MemorySlices != b.MemorySlices {
			return a.MemorySlices > b.MemorySlices
		}
		if a.ComputeSlices != b.ComputeSlices {
			return a.ComputeSlices > b.ComputeSlices
		}
		return items[i].req.ID < items[j].req.ID
	})

	var gpus []*GPU
	for _, it := range items {
		placed := false
		for _, g := range gpus {
			if g.place(model, it.profile, it.req.ID) {
				placed = true
				break
			}
		}
		if !placed {
			g := newGPU(model)
			if !g.place(model, it.profile, it.req.ID) {
				return nil, fmt.Errorf("request %q: profile %q does not fit on an empty %s", it.req.ID, it.profile.Name, model.Name)
			}
			gpus = append(gpus, g)
		}
	}
	return gpus, nil
}
