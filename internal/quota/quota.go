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

// Package quota holds the per-tenant MIG quota arithmetic.
package quota

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
)

// Usage is the MIG capacity held by one or more deployments.
type Usage struct {
	// Slices counts MIG instances by profile.
	Slices map[string]int64
	// MemoryGiB is the sum of the nominal memory of those instances.
	MemoryGiB int64
}

// Add records count instances of profile.
func (u *Usage) Add(profile string, count int64) error {
	mem, err := packing.NominalMemoryGiB(profile)
	if err != nil {
		return err
	}
	if u.Slices == nil {
		u.Slices = map[string]int64{}
	}
	u.Slices[profile] += count
	u.MemoryGiB += mem * count
	return nil
}

// Plus returns the sum of u and o.
func (u Usage) Plus(o Usage) Usage {
	out := Usage{Slices: map[string]int64{}, MemoryGiB: u.MemoryGiB + o.MemoryGiB}
	for p, n := range u.Slices {
		out.Slices[p] += n
	}
	for p, n := range o.Slices {
		out.Slices[p] += n
	}
	return out
}

// IncreasesOver reports whether u needs more of any profile, or more memory,
// than base. Only increases are subject to quota; shrinking is always allowed.
func (u Usage) IncreasesOver(base Usage) bool {
	if u.MemoryGiB > base.MemoryGiB {
		return true
	}
	for p, n := range u.Slices {
		if n > base.Slices[p] {
			return true
		}
	}
	return false
}

// Profiles returns the profiles with a non-zero count, sorted.
func (u Usage) Profiles() []string {
	var out []string
	for p, n := range u.Slices {
		if n != 0 {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Limits is one TenantQuota.
type Limits struct {
	// Name identifies the quota in messages.
	Name string
	// Slices caps instances per profile. Profiles that are not listed are not
	// capped by count (the memory cap still applies).
	Slices map[string]int64
	// MaxMemoryGiB caps total nominal MIG memory; nil means no cap.
	MaxMemoryGiB *int64
}

// Violation describes one exceeded limit.
type Violation struct {
	Quota     string
	Resource  string
	Requested int64
	InUse     int64
	Limit     int64
}

func (v Violation) String() string {
	return fmt.Sprintf("TenantQuota %s: %s would be %d (this deployment %d + other deployments %d), limit %d",
		v.Quota, v.Resource, v.Requested+v.InUse, v.Requested, v.InUse, v.Limit)
}

// Check returns the limits that others plus proposed would exceed.
func Check(limits []Limits, others, proposed Usage) []Violation {
	total := others.Plus(proposed)
	var out []Violation
	for _, l := range limits {
		for _, p := range total.Profiles() {
			limit, ok := l.Slices[p]
			if ok && total.Slices[p] > limit {
				out = append(out, Violation{Quota: l.Name, Resource: p + " slices", Requested: proposed.Slices[p], InUse: others.Slices[p], Limit: limit})
			}
		}
		if l.MaxMemoryGiB != nil && total.MemoryGiB > *l.MaxMemoryGiB {
			out = append(out, Violation{Quota: l.Name, Resource: "GPU memory GiB", Requested: proposed.MemoryGiB, InUse: others.MemoryGiB, Limit: *l.MaxMemoryGiB})
		}
	}
	return out
}

// Message joins violations into one condition message.
func Message(vs []Violation) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.String()
	}
	return strings.Join(parts, "; ")
}
