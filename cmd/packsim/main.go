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

// Command packsim compares the GPUs a sample fleet needs when every replica
// gets a whole GPU with the GPUs it needs when replicas are packed onto the
// MIG instances the planner chooses.
//
// With -explain it instead prints the memory estimate of every variant of one
// catalog model and the smallest MIG profile each fits.
//
// It is a static allocation calculation from the planner's memory estimates.
// It does not load models, run inference or measure GPU utilization.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/nitishsjsucs/llm-deployment-operator/internal/packing"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/planner"
)

type workload struct {
	name      string
	catalog   string
	replicas  int
	variants  []planner.Variant
	budgetGiB float64
}

var (
	fp16  = planner.Variant{Quantization: planner.FP16, Bits: 16}
	awq   = planner.Variant{Quantization: planner.AWQ, Bits: 4}
	gptq4 = planner.Variant{Quantization: planner.GPTQ, Bits: 4}
	gptq8 = planner.Variant{Quantization: planner.GPTQ, Bits: 8}
)

// sampleFleet is an illustrative mix of chat, RAG and batch deployments.
var sampleFleet = []workload{
	{name: "llama-3.1-8b-chat", catalog: "llama-3.1-8b", replicas: 8, variants: []planner.Variant{fp16, awq, gptq4}, budgetGiB: 20},
	{name: "qwen2.5-7b-assistant", catalog: "qwen2.5-7b", replicas: 6, variants: []planner.Variant{fp16, awq}, budgetGiB: 20},
	{name: "qwen2.5-14b-rag", catalog: "qwen2.5-14b", replicas: 3, variants: []planner.Variant{fp16, gptq4}, budgetGiB: 40},
	{name: "llama-3.1-70b-reasoning", catalog: "llama-3.1-70b", replicas: 2, variants: []planner.Variant{awq}},
	{name: "qwen2.5-72b-batch", catalog: "qwen2.5-72b", replicas: 1, variants: []planner.Variant{gptq4}},
}

func main() {
	gpuName := flag.String("gpu", "A100-80GB", "GPU type: "+strings.Join(packing.GPUNames(), ", "))
	maxModelLen := flag.Int64("max-model-len", 8192, "context length per sequence")
	maxNumSeqs := flag.Int64("max-num-seqs", 8, "concurrent sequences per replica")
	explainModel := flag.String("explain", "", "print the per-variant estimate for one catalog model: "+strings.Join(planner.CatalogIDs(), ", "))
	flag.Parse()

	gpu, ok := packing.LookupGPU(*gpuName)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown GPU type %q\n", *gpuName)
		os.Exit(2)
	}
	wl := planner.Workload{MaxModelLen: *maxModelLen, MaxNumSeqs: *maxNumSeqs}
	var err error
	if *explainModel != "" {
		err = explain(os.Stdout, gpu, wl, *explainModel)
	} else {
		err = run(os.Stdout, gpu, wl, sampleFleet)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func gib(bytes int64) float64 { return float64(bytes) / planner.GiB }

// explain prints how each variant of a catalog model is sized.
func explain(out io.Writer, gpu packing.GPUModel, wl planner.Workload, id string) error {
	arch, ok := planner.Lookup(id)
	if !ok {
		return fmt.Errorf("unknown catalog entry %q", id)
	}
	tokens := wl.MaxModelLen * wl.MaxNumSeqs
	fmt.Fprintf(out, "%s on %s, max-model-len=%d max-num-seqs=%d\n", id, gpu.Name, wl.MaxModelLen, wl.MaxNumSeqs)
	fmt.Fprintf(out, "parameters: %d, of which embedding + LM head (kept in 16-bit when quantized): %d\n", arch.Params, arch.EmbeddingParams())
	fmt.Fprintf(out, "KV cache: 2 x %d layers x %d KV heads x %d head dim x %d bytes = %d bytes/token, x %d tokens\n\n",
		arch.Layers, arch.KVHeads, arch.HeadDim, planner.KVCacheBytesPerElement, planner.KVCacheBytesPerToken(arch), tokens)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VARIANT\tWEIGHTS\tKV CACHE\tOVERHEAD\tESTIMATE\tSMALLEST PROFILE\tPROFILE USABLE\tPER GPU")
	for _, v := range []planner.Variant{fp16, gptq8, awq, gptq4} {
		e := planner.EstimateMemory(arch, v, wl)
		profile, usable, perGPU := "none", "-", "-"
		if plan, err := planner.Choose(planner.Request{Arch: arch, Variants: []planner.Variant{v}, GPU: gpu, Workload: wl}); err == nil {
			profile = plan.Profile.Name
			usable = fmt.Sprintf("%.2f", planner.UsableGiB(plan.Profile))
			perGPU = fmt.Sprint(plan.ReplicasPerGPU)
		}
		fmt.Fprintf(tw, "%s\t%.2f\t%.2f\t%.2f\t%.2f\t%s\t%s\t%s\n", v,
			gib(e.WeightsBytes), gib(e.KVCacheBytes), gib(e.OverheadBytes), e.TotalGiB(), profile, usable, perGPU)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(out, "(memory in GiB; PROFILE USABLE is %.0f%% of the profile's memory)\n", planner.GPUMemoryUtilization*100)
	return nil
}

func run(out io.Writer, gpu packing.GPUModel, wl planner.Workload, fleet []workload) error {
	fmt.Fprintln(out, "Static allocation calculation: per-replica memory comes from the planner's estimate")
	fmt.Fprintln(out, "(weights + KV cache for max-num-seqs sequences at max-model-len + fixed runtime overhead).")
	fmt.Fprintln(out, "No model was loaded and no GPU utilization was measured.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "GPU %s: %.0f GiB, %d compute slices, %d memory slices\n", gpu.Name, gpu.MemoryGiB, gpu.ComputeSlices, gpu.MemorySlices)
	fmt.Fprintf(out, "max-model-len=%d max-num-seqs=%d gpu-memory-utilization=%.2f\n\n", wl.MaxModelLen, wl.MaxNumSeqs, planner.GPUMemoryUtilization)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKLOAD\tREPLICAS\tVARIANT\tWEIGHTS\tKV CACHE\tOVERHEAD\tESTIMATE\tMIG PROFILE\tPER GPU")
	var (
		reqs        []packing.Request
		estimateSum float64
		replicas    int
	)
	for _, w := range fleet {
		arch, ok := planner.Lookup(w.catalog)
		if !ok {
			return fmt.Errorf("%s: unknown catalog entry %q", w.name, w.catalog)
		}
		plan, err := planner.Choose(planner.Request{Arch: arch, Variants: w.variants, GPU: gpu, MaxMemoryGiBPerReplica: w.budgetGiB, Workload: wl})
		if err != nil {
			return fmt.Errorf("%s: %w", w.name, err)
		}
		e := plan.Estimate
		fmt.Fprintf(tw, "%s\t%d\t%s\t%.2f\t%.2f\t%.2f\t%.2f\t%s\t%d\n", w.name, w.replicas, plan.Variant,
			gib(e.WeightsBytes), gib(e.KVCacheBytes), gib(e.OverheadBytes), e.TotalGiB(), plan.Profile.Name, plan.ReplicasPerGPU)
		for i := 0; i < w.replicas; i++ {
			reqs = append(reqs, packing.Request{ID: fmt.Sprintf("%s/%d", w.name, i), Profile: plan.Profile.Name})
		}
		estimateSum += e.TotalGiB() * float64(w.replicas)
		replicas += w.replicas
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out, "(memory in GiB)")
	fmt.Fprintln(out)

	gpus, err := packing.Pack(gpu, reqs)
	if err != nil {
		return err
	}
	var migGiB int64
	computeUsed := 0
	for _, g := range gpus {
		for _, in := range g.Instances {
			n, err := packing.NominalMemoryGiB(in.Profile.Name)
			if err != nil {
				return err
			}
			migGiB += n
		}
		computeUsed += g.ComputeSlicesUsed()
	}

	naiveGPUs := replicas
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ALLOCATION\tGPUS\tREPLICAS\tESTIMATE / GPU MEMORY\tMIG MEMORY / GPU MEMORY\tCOMPUTE SLICES IN USE")
	fmt.Fprintf(tw, "one replica per whole GPU\t%d\t%d\t%.1f%%\t-\t-\n", naiveGPUs, replicas,
		100*estimateSum/(float64(naiveGPUs)*gpu.MemoryGiB))
	fmt.Fprintf(tw, "MIG-packed (first-fit decreasing)\t%d\t%d\t%.1f%%\t%.1f%%\t%d/%d\n", len(gpus), replicas,
		100*estimateSum/(float64(len(gpus))*gpu.MemoryGiB),
		100*float64(migGiB)/(float64(len(gpus))*gpu.MemoryGiB),
		computeUsed, len(gpus)*gpu.ComputeSlices)
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "ESTIMATE / GPU MEMORY: sum of per-replica estimates / total memory of the GPUs needed.")
	fmt.Fprintln(out, "MIG MEMORY / GPU MEMORY: nominal memory of the allocated MIG instances / total memory of the GPUs needed.")
	fmt.Fprintln(out)

	fmt.Fprintln(out, "MIG layout:")
	for i, g := range gpus {
		var parts []string
		for _, in := range g.Instances {
			parts = append(parts, fmt.Sprintf("%s@%d %s", in.Profile.Name, in.Start, in.RequestID))
		}
		fmt.Fprintf(out, "  gpu%-2d %s\n", i, strings.Join(parts, ", "))
	}
	return nil
}
