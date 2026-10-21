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
	"reflect"
	"testing"
)

func mustGPU(t *testing.T, name string) GPUModel {
	t.Helper()
	m, ok := LookupGPU(name)
	if !ok {
		t.Fatalf("unknown GPU %s", name)
	}
	return m
}

func TestInstancesPerGPU(t *testing.T) {
	tests := []struct {
		gpu  string
		want map[string]int
	}{
		{"A100-80GB", map[string]int{"1g.10gb": 7, "1g.20gb": 4, "2g.20gb": 3, "3g.40gb": 2, "4g.40gb": 1, "7g.80gb": 1}},
		{"A100-40GB", map[string]int{"1g.5gb": 7, "1g.10gb": 4, "2g.10gb": 3, "3g.20gb": 2, "4g.20gb": 1, "7g.40gb": 1}},
		{"H100-80GB", map[string]int{"1g.10gb": 7, "1g.20gb": 4, "2g.20gb": 3, "3g.40gb": 2, "4g.40gb": 1, "7g.80gb": 1}},
	}
	for _, tt := range tests {
		m := mustGPU(t, tt.gpu)
		if len(m.Profiles) != len(tt.want) {
			t.Errorf("%s: got %d profiles, want %d", tt.gpu, len(m.Profiles), len(tt.want))
		}
		for name, want := range tt.want {
			p, ok := m.Profile(name)
			if !ok {
				t.Errorf("%s: missing profile %s", tt.gpu, name)
				continue
			}
			if got := m.InstancesPerGPU(p); got != want {
				t.Errorf("%s %s: InstancesPerGPU = %d, want %d", tt.gpu, name, got, want)
			}
			if p.MaxInstances != want {
				t.Errorf("%s %s: MaxInstances = %d, want %d", tt.gpu, name, p.MaxInstances, want)
			}
		}
	}
}

func TestProfileTablesAreConsistent(t *testing.T) {
	for _, name := range GPUNames() {
		m := mustGPU(t, name)
		for i, p := range m.Profiles {
			if p.ComputeSlices > m.ComputeSlices || p.MemorySlices > m.MemorySlices {
				t.Errorf("%s %s exceeds GPU slices", name, p.Name)
			}
			if len(p.Placements) < p.MaxInstances {
				t.Errorf("%s %s: %d placements for %d instances", name, p.Name, len(p.Placements), p.MaxInstances)
			}
			for _, s := range p.Placements {
				if s+p.MemorySlices > m.MemorySlices {
					t.Errorf("%s %s: placement %d overflows", name, p.Name, s)
				}
			}
			if p.MemoryGiB > m.MemoryGiB {
				t.Errorf("%s %s: memory %.2f exceeds GPU memory", name, p.Name, p.MemoryGiB)
			}
			if i > 0 && p.MemoryGiB < m.Profiles[i-1].MemoryGiB {
				t.Errorf("%s: profiles not ordered by memory at %s", name, p.Name)
			}
			nominal, err := NominalMemoryGiB(p.Name)
			if err != nil {
				t.Fatal(err)
			}
			if want := int64(m.MemoryGiB) * int64(p.MemorySlices) / int64(m.MemorySlices); nominal != want {
				t.Errorf("%s %s: nominal memory %d, want %d from memory slices", name, p.Name, nominal, want)
			}
		}
	}
}

func repeat(profile string, n int, prefix string) []Request {
	out := make([]Request, n)
	for i := range out {
		out[i] = Request{ID: fmt.Sprintf("%s-%d", prefix, i), Profile: profile}
	}
	return out
}

func concat(rs ...[]Request) []Request {
	var out []Request
	for _, r := range rs {
		out = append(out, r...)
	}
	return out
}

func TestPackA100_80GB(t *testing.T) {
	m := mustGPU(t, "A100-80GB")
	tests := []struct {
		name     string
		reqs     []Request
		wantGPUs int
	}{
		{"four 1g.20gb share one GPU", repeat("1g.20gb", 4, "a"), 1},
		{"fifth 1g.20gb opens a second GPU", repeat("1g.20gb", 5, "a"), 2},
		{"seven 1g.10gb per GPU", repeat("1g.10gb", 8, "a"), 2},
		{"three 2g.20gb plus 1g.10gb", concat(repeat("2g.20gb", 3, "a"), repeat("1g.10gb", 1, "b")), 1},
		{"4g.40gb plus 3g.40gb", concat(repeat("3g.40gb", 1, "a"), repeat("4g.40gb", 1, "b")), 1},
		{"only one 4g.40gb per GPU", repeat("4g.40gb", 2, "a"), 2},
		{"two 3g.40gb fill the memory slices", concat(repeat("3g.40gb", 2, "a"), repeat("1g.10gb", 1, "b")), 2},
		{"4g.40gb plus two 1g.20gb", concat(repeat("1g.20gb", 2, "a"), repeat("4g.40gb", 1, "b")), 1},
		{"2g.20gb plus three 1g.20gb", concat(repeat("1g.20gb", 3, "a"), repeat("2g.20gb", 1, "b")), 1},
		{"7g.80gb is a whole GPU", concat(repeat("7g.80gb", 1, "a"), repeat("1g.10gb", 1, "b")), 2},
		{"empty", nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gpus, err := Pack(m, tt.reqs)
			if err != nil {
				t.Fatal(err)
			}
			if len(gpus) != tt.wantGPUs {
				t.Fatalf("got %d GPUs, want %d: %s", len(gpus), tt.wantGPUs, layout(gpus))
			}
			placed := 0
			for _, g := range gpus {
				placed += len(g.Instances)
				if g.ComputeSlicesUsed() > m.ComputeSlices || g.MemorySlicesUsed() > m.MemorySlices {
					t.Errorf("GPU over capacity: %s", layout(gpus))
				}
			}
			if placed != len(tt.reqs) {
				t.Errorf("placed %d of %d requests", placed, len(tt.reqs))
			}
		})
	}
}

func TestPackPlacementOffsets(t *testing.T) {
	m := mustGPU(t, "A100-80GB")
	gpus, err := Pack(m, concat(repeat("1g.20gb", 2, "small"), repeat("4g.40gb", 1, "big")))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, in := range gpus[0].Instances {
		got[in.RequestID] = in.Start
	}
	want := map[string]int{"big-0": 0, "small-0": 4, "small-1": 6}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
	if gpus[0].ComputeSlicesUsed() != 6 || gpus[0].MemorySlicesUsed() != 8 {
		t.Errorf("used compute=%d memory=%d, want 6 and 8", gpus[0].ComputeSlicesUsed(), gpus[0].MemorySlicesUsed())
	}
	if got := gpus[0].AllocatedMemoryGiB(); got != 39.25+2*19.5 {
		t.Errorf("allocated memory = %.2f", got)
	}
}

func TestPackIsDeterministic(t *testing.T) {
	m := mustGPU(t, "A100-80GB")
	a := concat(repeat("1g.20gb", 5, "x"), repeat("3g.40gb", 3, "y"), repeat("1g.10gb", 4, "z"))
	b := make([]Request, len(a))
	for i := range a {
		b[i] = a[len(a)-1-i]
	}
	ga, err := Pack(m, a)
	if err != nil {
		t.Fatal(err)
	}
	gb, err := Pack(m, b)
	if err != nil {
		t.Fatal(err)
	}
	if layout(ga) != layout(gb) {
		t.Errorf("layout depends on input order:\n%s\n%s", layout(ga), layout(gb))
	}
}

func TestPackUnknownProfile(t *testing.T) {
	m := mustGPU(t, "A100-40GB")
	if _, err := Pack(m, repeat("1g.20gb", 1, "a")); err == nil {
		t.Fatal("expected error for a profile that does not exist on A100-40GB")
	}
}

func TestNominalMemoryGiB(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1g.20gb", 20, false},
		{"7g.80gb", 80, false},
		{"1g.5gb", 5, false},
		{"1g.10gb+me", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		got, err := NominalMemoryGiB(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("NominalMemoryGiB(%q) = %d, %v; want %d, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func layout(gpus []*GPU) string {
	s := ""
	for i, g := range gpus {
		s += fmt.Sprintf("gpu%d:", i)
		for _, in := range g.Instances {
			s += fmt.Sprintf(" %s@%d(%s)", in.Profile.Name, in.Start, in.RequestID)
		}
		s += "\n"
	}
	return s
}
