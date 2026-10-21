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

package main

import (
	"bytes"
	"regexp"
	"testing"

	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/planner"
)

func TestRunSampleFleet(t *testing.T) {
	gpu, _ := packing.LookupGPU("A100-80GB")
	var out bytes.Buffer
	if err := run(&out, gpu, planner.Workload{MaxModelLen: 8192, MaxNumSeqs: 8}, sampleFleet); err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`No model was loaded and no GPU utilization was measured`,
		`llama-3.1-8b-chat\s+8\s+awq-int4\s.*\s1g\.20gb\s+4\n`,
		`one replica per whole GPU\s+20\s+20\s`,
		`MIG-packed \(first-fit decreasing\)\s+8\s+20\s`,
	} {
		if !regexp.MustCompile(re).Match(out.Bytes()) {
			t.Errorf("output does not match %q:\n%s", re, out.String())
		}
	}
}

func TestRunRejectsUnknownModel(t *testing.T) {
	gpu, _ := packing.LookupGPU("A100-80GB")
	fleet := []workload{{name: "x", catalog: "nope", replicas: 1, variants: []planner.Variant{awq}}}
	if err := run(&bytes.Buffer{}, gpu, planner.Workload{MaxModelLen: 8192, MaxNumSeqs: 8}, fleet); err == nil {
		t.Error("expected an error for an unknown catalog entry")
	}
}

func TestExplain(t *testing.T) {
	gpu, _ := packing.LookupGPU("A100-80GB")
	var out bytes.Buffer
	if err := explain(&out, gpu, planner.Workload{MaxModelLen: 8192, MaxNumSeqs: 8}, "llama-3.1-8b"); err != nil {
		t.Fatal(err)
	}
	for _, re := range []string{
		`131072 bytes/token, x 65536 tokens`,
		`fp16\s+14\.96\s+8\.00\s+1\.50\s+24\.46\s+3g\.40gb\s+35\.33\s+2\n`,
		`awq-int4\s+5\.33\s+8\.00\s+1\.50\s+14\.83\s+1g\.20gb\s+17\.55\s+4\n`,
	} {
		if !regexp.MustCompile(re).Match(out.Bytes()) {
			t.Errorf("output does not match %q:\n%s", re, out.String())
		}
	}
	if err := explain(&out, gpu, planner.Workload{MaxModelLen: 8192, MaxNumSeqs: 8}, "nope"); err == nil {
		t.Error("expected an error for an unknown model")
	}
}
