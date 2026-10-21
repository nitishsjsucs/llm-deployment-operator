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

package quota

import (
	"strings"
	"testing"
)

func usage(t *testing.T, pairs ...any) Usage {
	t.Helper()
	var u Usage
	for i := 0; i < len(pairs); i += 2 {
		if err := u.Add(pairs[i].(string), int64(pairs[i+1].(int))); err != nil {
			t.Fatal(err)
		}
	}
	return u
}

func ptr(v int64) *int64 { return &v }

func TestUsageAdd(t *testing.T) {
	u := usage(t, "1g.20gb", 4, "3g.40gb", 1, "1g.20gb", 1)
	if u.Slices["1g.20gb"] != 5 || u.Slices["3g.40gb"] != 1 {
		t.Errorf("slices = %v", u.Slices)
	}
	if u.MemoryGiB != 5*20+40 {
		t.Errorf("memory = %d, want 140", u.MemoryGiB)
	}
	var bad Usage
	if err := bad.Add("not-a-profile", 1); err == nil {
		t.Error("expected an error for an invalid profile")
	}
}

func TestIncreasesOver(t *testing.T) {
	base := usage(t, "1g.20gb", 4)
	tests := []struct {
		name string
		u    Usage
		want bool
	}{
		{"same", usage(t, "1g.20gb", 4), false},
		{"scale down", usage(t, "1g.20gb", 2), false},
		{"scale up", usage(t, "1g.20gb", 5), true},
		{"new profile with less memory overall", usage(t, "1g.10gb", 1), true},
		{"empty", Usage{}, false},
	}
	for _, tt := range tests {
		if got := tt.u.IncreasesOver(base); got != tt.want {
			t.Errorf("%s: IncreasesOver = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name     string
		limits   []Limits
		others   Usage
		proposed Usage
		want     []string
	}{
		{
			name:     "no quota means no limit",
			others:   usage(t, "1g.20gb", 100),
			proposed: usage(t, "1g.20gb", 100),
		},
		{
			name:     "within slice limit",
			limits:   []Limits{{Name: "team-a", Slices: map[string]int64{"1g.20gb": 8}}},
			others:   usage(t, "1g.20gb", 4),
			proposed: usage(t, "1g.20gb", 4),
		},
		{
			name:     "slice limit exceeded",
			limits:   []Limits{{Name: "team-a", Slices: map[string]int64{"1g.20gb": 8}}},
			others:   usage(t, "1g.20gb", 6),
			proposed: usage(t, "1g.20gb", 4),
			want:     []string{"TenantQuota team-a: 1g.20gb slices would be 10 (this deployment 4 + other deployments 6), limit 8"},
		},
		{
			name:     "unlisted profiles are only bound by memory",
			limits:   []Limits{{Name: "team-a", Slices: map[string]int64{"1g.20gb": 8}, MaxMemoryGiB: ptr(100)}},
			proposed: usage(t, "3g.40gb", 2),
		},
		{
			name:     "memory limit exceeded",
			limits:   []Limits{{Name: "team-a", MaxMemoryGiB: ptr(160)}},
			others:   usage(t, "7g.80gb", 1),
			proposed: usage(t, "1g.20gb", 5),
			want:     []string{"TenantQuota team-a: GPU memory GiB would be 180 (this deployment 100 + other deployments 80), limit 160"},
		},
		{
			name: "every matching quota must hold",
			limits: []Limits{
				{Name: "loose", Slices: map[string]int64{"1g.20gb": 100}},
				{Name: "tight", Slices: map[string]int64{"1g.20gb": 2}},
			},
			proposed: usage(t, "1g.20gb", 3),
			want:     []string{"TenantQuota tight: 1g.20gb slices would be 3 (this deployment 3 + other deployments 0), limit 2"},
		},
		{
			name:     "a zero limit forbids the profile",
			limits:   []Limits{{Name: "team-a", Slices: map[string]int64{"7g.80gb": 0}}},
			proposed: usage(t, "7g.80gb", 1),
			want:     []string{"TenantQuota team-a: 7g.80gb slices would be 1 (this deployment 1 + other deployments 0), limit 0"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vs := Check(tt.limits, tt.others, tt.proposed)
			if len(vs) != len(tt.want) {
				t.Fatalf("got %d violations (%s), want %d", len(vs), Message(vs), len(tt.want))
			}
			for i, v := range vs {
				if v.String() != tt.want[i] {
					t.Errorf("violation %d = %q, want %q", i, v.String(), tt.want[i])
				}
			}
			if len(vs) > 1 && !strings.Contains(Message(vs), "; ") {
				t.Errorf("Message should join violations: %q", Message(vs))
			}
		})
	}
}
