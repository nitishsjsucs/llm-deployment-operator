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

package controller

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
)

func TestInitialCreate(t *testing.T) {
	md := llamaDeployment("chat", "team-a", 4)
	h := newHarness(t, md)
	h.reconcile(md)

	got := h.get(md)
	rev := got.Status.StableRevision
	if rev == "" {
		t.Fatal("stable revision not set")
	}
	if v := got.Status.SelectedVariant; v == nil || v.Quantization != servingv1alpha1.QuantizationAWQ || v.Bits != 4 {
		t.Errorf("selected variant = %+v, want awq int4", v)
	}
	if got.Status.MIGProfile != "1g.20gb" || got.Status.ReplicasPerGPU != 4 {
		t.Errorf("profile = %s, replicas per GPU = %d; want 1g.20gb and 4", got.Status.MIGProfile, got.Status.ReplicasPerGPU)
	}
	if e := got.Status.MemoryEstimate; e == nil || e.KVCacheMiB != 8192 || e.TotalMiB > e.ProfileMiB {
		t.Errorf("memory estimate = %+v", e)
	}
	wantAlloc := []servingv1alpha1.SliceAllocation{{Revision: rev, Profile: "1g.20gb", Count: 4}}
	if !reflect.DeepEqual(got.Status.AllocatedSlices, wantAlloc) {
		t.Errorf("allocated = %+v, want %+v", got.Status.AllocatedSlices, wantAlloc)
	}
	if c := condition(got, servingv1alpha1.ConditionPlanned); c == nil || c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "awq-int4 on 1g.20gb") {
		t.Errorf("Planned = %+v", c)
	}
	if got.Status.Phase != servingv1alpha1.PhasePending {
		t.Errorf("phase = %s, want Pending until KServe reports Ready", got.Status.Phase)
	}

	// LLMInferenceService.
	svc, ok := h.llmisvc(md, rev)
	if !ok {
		t.Fatal("LLMInferenceService not created")
	}
	if !metav1.IsControlledBy(svc, got) {
		t.Error("LLMInferenceService is not owned by the ModelDeployment")
	}
	if uri, _, _ := unstructured.NestedString(svc.Object, "spec", "model", "uri"); uri != "hf://hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4" {
		t.Errorf("model.uri = %s", uri)
	}
	if n := replicasOf(t, svc); n != 4 {
		t.Errorf("replicas = %d, want 4", n)
	}
	containers, _, _ := unstructured.NestedSlice(svc.Object, "spec", "template", "containers")
	args, _, _ := unstructured.NestedStringSlice(containers[0].(map[string]any), "args")
	for _, want := range [][2]string{{"--quantization", "awq_marlin"}, {"--max-model-len", "8192"}, {"--max-num-seqs", "8"}, {"--gpu-memory-utilization", "0.90"}} {
		i := slices.Index(args, want[0])
		if i < 0 || args[i+1] != want[1] {
			t.Errorf("args %v missing %s %s", args, want[0], want[1])
		}
	}
	claimRef, _, _ := unstructured.NestedSlice(svc.Object, "spec", "template", "resourceClaims")
	if len(claimRef) != 1 || claimRef[0].(map[string]any)["resourceClaimTemplateName"] != render.ClaimTemplateName(md.Name, rev) {
		t.Errorf("resourceClaims = %v", claimRef)
	}

	// ResourceClaimTemplate.
	rct, ok := h.claimTemplate(md, rev)
	if !ok {
		t.Fatal("ResourceClaimTemplate not created")
	}
	ex := rct.Spec.Spec.Devices.Requests[0].Exactly
	if ex.DeviceClassName != "mig.nvidia.com" || ex.Selectors[0].CEL.Expression != "device.attributes['gpu.nvidia.com'].profile == '1g.20gb'" {
		t.Errorf("claim request = %+v", ex)
	}

	// HTTPRoute.
	route, ok := h.route(md)
	if !ok {
		t.Fatal("HTTPRoute not created")
	}
	if b := backendsOf(t, route); !reflect.DeepEqual(b, []weightedBackend{{pool(md, rev), 100}}) {
		t.Errorf("backends = %v", b)
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if p := parents[0].(map[string]any); p["name"] != "kserve-ingress-gateway" || p["namespace"] != "kserve" {
		t.Errorf("parentRefs = %v, want the default Gateway", parents)
	}
	gw, _, _ := unstructured.NestedSlice(svc.Object, "spec", "router", "gateway", "refs")
	if len(gw) != 1 || gw[0].(map[string]any)["name"] != "kserve-ingress-gateway" {
		t.Errorf("router.gateway.refs = %v", gw)
	}
	if !h.snapshotExists(md, rev) {
		t.Error("ControllerRevision not created")
	}
	if ev := h.events(); !hasEvent(ev, "Deployed") {
		t.Errorf("events = %v, want Deployed", ev)
	}

	// KServe reports Ready.
	h.setReady(md, rev, true)
	h.reconcile(md)
	got = h.get(md)
	if got.Status.Phase != servingv1alpha1.PhaseReady {
		t.Errorf("phase = %s, want Ready", got.Status.Phase)
	}
	if c := condition(got, servingv1alpha1.ConditionReady); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v", c)
	}
}

func TestPlanFailure(t *testing.T) {
	md := llamaDeployment("tiny", "team-a", 1)
	md.Spec.GPUBudget.MaxMemoryGiBPerReplica = ptr.To[int32](8)
	h := newHarness(t, md)
	h.reconcile(md)

	got := h.get(md)
	if got.Status.Phase != servingv1alpha1.PhaseFailed {
		t.Errorf("phase = %s, want Failed", got.Status.Phase)
	}
	c := condition(got, servingv1alpha1.ConditionPlanned)
	if c == nil || c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "no variant fits") {
		t.Errorf("Planned = %+v", c)
	}
	if _, ok := h.route(md); ok {
		t.Error("no HTTPRoute should be created without a plan")
	}
}

func TestQuotaExceeded(t *testing.T) {
	tq := &servingv1alpha1.TenantQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
		Spec: servingv1alpha1.TenantQuotaSpec{
			Tenant:    "team-a",
			MIGSlices: []servingv1alpha1.SliceLimit{{Profile: "1g.20gb", Max: 6}},
		},
	}
	peer := llamaDeployment("peer", "team-a-prod", 4)
	peer.Status.AllocatedSlices = []servingv1alpha1.SliceAllocation{{Revision: "aaaa0000", Profile: "1g.20gb", Count: 4}}

	t.Run("create beyond quota is not applied", func(t *testing.T) {
		md := llamaDeployment("big", "team-a", 4)
		h := newHarness(t, tq.DeepCopy(), peer.DeepCopy(), md)
		h.reconcile(md)

		got := h.get(md)
		c := condition(got, servingv1alpha1.ConditionQuotaExceeded)
		if c == nil || c.Status != metav1.ConditionTrue {
			t.Fatalf("QuotaExceeded = %+v", c)
		}
		if want := "TenantQuota team-a: 1g.20gb slices would be 8 (this deployment 4 + other deployments 4), limit 6"; c.Message != want {
			t.Errorf("message = %q, want %q", c.Message, want)
		}
		if got.Status.StableRevision != "" || len(got.Status.AllocatedSlices) != 0 {
			t.Errorf("nothing should be deployed: %+v", got.Status)
		}
		if _, ok := h.route(md); ok {
			t.Error("HTTPRoute created despite quota")
		}
		if got.Status.Phase != servingv1alpha1.PhasePending {
			t.Errorf("phase = %s", got.Status.Phase)
		}
		if ev := h.events(); !hasEvent(ev, "QuotaExceeded") {
			t.Errorf("events = %v", ev)
		}
	})

	t.Run("scale-up beyond quota keeps existing replicas", func(t *testing.T) {
		md := llamaDeployment("small", "team-a", 2)
		h := newHarness(t, tq.DeepCopy(), peer.DeepCopy(), md)
		h.reconcile(md)
		rev := h.get(md).Status.StableRevision
		if c := condition(h.get(md), servingv1alpha1.ConditionQuotaExceeded); c.Status != metav1.ConditionFalse {
			t.Fatalf("2 + 4 slices fit a quota of 6: %+v", c)
		}

		h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) { s.Replicas = ptr.To[int32](3) })
		h.reconcile(md)
		got := h.get(md)
		if c := condition(got, servingv1alpha1.ConditionQuotaExceeded); c.Status != metav1.ConditionTrue {
			t.Errorf("QuotaExceeded = %+v", c)
		}
		svc, _ := h.llmisvc(md, rev)
		if n := replicasOf(t, svc); n != 2 {
			t.Errorf("replicas = %d, want 2 kept", n)
		}
		if a := got.Status.AllocatedSlices; len(a) != 1 || a[0].Count != 2 {
			t.Errorf("allocated = %+v", a)
		}

		// Raising the quota lets the scale-up through.
		raised := &servingv1alpha1.TenantQuota{}
		if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(tq), raised); err != nil {
			t.Fatal(err)
		}
		raised.Spec.MIGSlices[0].Max = 8
		if err := h.c.Update(h.ctx, raised); err != nil {
			t.Fatal(err)
		}
		if reqs := h.r.deploymentsForQuota(h.ctx, raised); len(reqs) != 2 {
			t.Errorf("quota change should requeue both deployments of the tenant, got %v", reqs)
		}
		h.reconcile(md)
		got = h.get(md)
		svc, _ = h.llmisvc(md, rev)
		if n := replicasOf(t, svc); n != 3 {
			t.Errorf("replicas = %d, want 3", n)
		}
		if c := condition(got, servingv1alpha1.ConditionQuotaExceeded); c.Status != metav1.ConditionFalse {
			t.Errorf("QuotaExceeded = %+v", c)
		}
	})

	t.Run("scale-down is allowed while over quota", func(t *testing.T) {
		md := llamaDeployment("shrink", "team-a", 2)
		h := newHarness(t, tq.DeepCopy(), peer.DeepCopy(), md)
		h.reconcile(md)
		rev := h.get(md).Status.StableRevision

		// The quota is lowered below current usage (4 + 2 slices).
		lowered := &servingv1alpha1.TenantQuota{}
		if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(tq), lowered); err != nil {
			t.Fatal(err)
		}
		lowered.Spec.MIGSlices[0].Max = 4
		if err := h.c.Update(h.ctx, lowered); err != nil {
			t.Fatal(err)
		}
		h.reconcile(md)
		svc, _ := h.llmisvc(md, rev)
		if n := replicasOf(t, svc); n != 2 {
			t.Errorf("replicas = %d; running replicas must not be evicted", n)
		}

		h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) { s.Replicas = ptr.To[int32](1) })
		h.reconcile(md)
		svc, _ = h.llmisvc(md, rev)
		if n := replicasOf(t, svc); n != 1 {
			t.Errorf("replicas = %d, want 1", n)
		}
		if c := condition(h.get(md), servingv1alpha1.ConditionQuotaExceeded); c.Status != metav1.ConditionFalse {
			t.Errorf("a scale-down must not be blocked: %+v", c)
		}
	})
}

func TestCapToAllocations(t *testing.T) {
	held := []servingv1alpha1.SliceAllocation{{Revision: "a", Profile: "1g.20gb", Count: 4}, {Revision: "b", Profile: "1g.20gb", Count: 1}}
	tests := []struct {
		name string
		in   []workload
		want []workload
	}{
		{"decrease passes", []workload{{"a", 2}}, []workload{{"a", 2}}},
		{"increase is capped", []workload{{"a", 6}, {"b", 2}}, []workload{{"a", 4}, {"b", 1}}},
		{"new revision is dropped", []workload{{"a", 4}, {"c", 1}}, []workload{{"a", 4}}},
	}
	for _, tt := range tests {
		if got := capToAllocations(tt.in, held); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

// startRollout deploys md, marks it Ready and changes the image so that a new
// revision is rolled out. It returns the stable and canary revisions.
func startRollout(t *testing.T, h *harness, md *servingv1alpha1.ModelDeployment) (stable, canary string) {
	t.Helper()
	h.reconcile(md)
	stable = h.get(md).Status.StableRevision
	h.setReady(md, stable, true)
	h.reconcile(md)
	h.events()

	h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) { s.Runtime.Image = "vllm/vllm-openai:v0.11.0" })
	res := h.reconcile(md)
	got := h.get(md)
	canary = got.Status.CanaryRevision
	if canary == "" || canary == stable || got.Status.StableRevision != stable {
		t.Fatalf("canary not started: %+v", got.Status)
	}
	if res.RequeueAfter != 30*time.Minute {
		t.Errorf("requeue = %s, want the 30m progress deadline", res.RequeueAfter)
	}
	return stable, canary
}

func TestCanaryProgressionAndPromotion(t *testing.T) {
	md := llamaDeployment("chat", "team-a", 4)
	h := newHarness(t, md)
	stable, canary := startRollout(t, h, md)

	got := h.get(md)
	if got.Status.Phase != servingv1alpha1.PhaseProgressing || got.Status.CanaryStep != -1 || got.Status.CanaryWeight != 0 {
		t.Errorf("warming status = phase %s step %d weight %d", got.Status.Phase, got.Status.CanaryStep, got.Status.CanaryWeight)
	}
	svc, ok := h.llmisvc(md, canary)
	if !ok {
		t.Fatal("canary LLMInferenceService not created")
	}
	if n := replicasOf(t, svc); n != 1 {
		t.Errorf("warming canary replicas = %d, want 1 (10%% of 4, rounded up)", n)
	}
	if _, ok := h.claimTemplate(md, canary); !ok {
		t.Error("canary ResourceClaimTemplate not created")
	}
	route, _ := h.route(md)
	if b := backendsOf(t, route); !reflect.DeepEqual(b, []weightedBackend{{pool(md, stable), 100}}) {
		t.Errorf("a warming canary must get no traffic: %v", b)
	}
	if ev := h.events(); !hasEvent(ev, "CanaryStarted") {
		t.Errorf("events = %v", ev)
	}
	wantAlloc := []servingv1alpha1.SliceAllocation{{Revision: stable, Profile: "1g.20gb", Count: 4}, {Revision: canary, Profile: "1g.20gb", Count: 1}}
	if !reflect.DeepEqual(got.Status.AllocatedSlices, wantAlloc) {
		t.Errorf("allocated = %+v, want %+v", got.Status.AllocatedSlices, wantAlloc)
	}

	steps := []struct {
		weight         int64
		canaryReplicas int64
	}{{10, 1}, {50, 2}, {100, 4}}
	h.setReady(md, canary, true)
	for i, step := range steps {
		res := h.reconcile(md)
		if res.RequeueAfter != 5*time.Minute {
			t.Errorf("step %d: requeue = %s, want 5m", i, res.RequeueAfter)
		}
		got = h.get(md)
		if int64(got.Status.CanaryWeight) != step.weight || got.Status.CanaryStep != int32(i) {
			t.Errorf("step %d: weight %d at index %d", i, got.Status.CanaryWeight, got.Status.CanaryStep)
		}
		route, _ = h.route(md)
		want := []weightedBackend{{pool(md, stable), 100 - step.weight}, {pool(md, canary), step.weight}}
		if b := backendsOf(t, route); !reflect.DeepEqual(b, want) {
			t.Errorf("step %d: backends = %v, want %v", i, b, want)
		}
		svc, _ = h.llmisvc(md, canary)
		if n := replicasOf(t, svc); n != step.canaryReplicas {
			t.Errorf("step %d: canary replicas = %d, want %d", i, n, step.canaryReplicas)
		}

		// Before the interval passes nothing changes.
		h.clock.Step(2 * time.Minute)
		if res := h.reconcile(md); res.RequeueAfter != 3*time.Minute {
			t.Errorf("step %d: requeue = %s, want the remaining 3m", i, res.RequeueAfter)
		}
		if h.get(md).Status.CanaryStep != int32(i) {
			t.Errorf("step %d advanced before the interval", i)
		}
		h.clock.Step(3 * time.Minute)
	}
	if ev := h.events(); !hasEvent(ev, "CanaryAdvanced") {
		t.Errorf("events = %v", ev)
	}

	// After the interval at 100% the canary is promoted.
	h.reconcile(md)
	got = h.get(md)
	if got.Status.StableRevision != canary || got.Status.CanaryRevision != "" || got.Status.CanaryWeight != 0 {
		t.Fatalf("not promoted: %+v", got.Status)
	}
	if got.Status.Phase != servingv1alpha1.PhaseReady {
		t.Errorf("phase = %s, want Ready", got.Status.Phase)
	}
	if _, ok := h.llmisvc(md, stable); ok {
		t.Error("old stable LLMInferenceService not deleted")
	}
	if _, ok := h.claimTemplate(md, stable); ok {
		t.Error("old stable ResourceClaimTemplate not deleted")
	}
	if h.snapshotExists(md, stable) {
		t.Error("old stable ControllerRevision not deleted")
	}
	route, _ = h.route(md)
	if b := backendsOf(t, route); !reflect.DeepEqual(b, []weightedBackend{{pool(md, canary), 100}}) {
		t.Errorf("backends after promotion = %v", b)
	}
	svc, _ = h.llmisvc(md, canary)
	if n := replicasOf(t, svc); n != 4 {
		t.Errorf("promoted replicas = %d", n)
	}
	if ev := h.events(); !hasEvent(ev, "CanaryPromoted") {
		t.Errorf("events = %v", ev)
	}
	if c := condition(got, servingv1alpha1.ConditionProgressing); c.Status != metav1.ConditionFalse {
		t.Errorf("Progressing = %+v", c)
	}
}

func TestRollbackOnDeadline(t *testing.T) {
	md := llamaDeployment("chat", "team-a", 4)
	h := newHarness(t, md)
	stable, canary := startRollout(t, h, md)

	// The canary becomes Ready and takes 10% ...
	h.setReady(md, canary, true)
	h.reconcile(md)
	if w := h.get(md).Status.CanaryWeight; w != 10 {
		t.Fatalf("weight = %d, want 10", w)
	}
	// ... then fails: it loses its traffic immediately.
	h.setReady(md, canary, false)
	h.clock.Step(time.Minute)
	if res := h.reconcile(md); res.RequeueAfter != 29*time.Minute {
		t.Errorf("requeue = %s, want the remaining 29m of the deadline", res.RequeueAfter)
	}
	route, _ := h.route(md)
	if b := backendsOf(t, route); !reflect.DeepEqual(b, []weightedBackend{{pool(md, stable), 100}}) {
		t.Errorf("an unready canary must get no traffic: %v", b)
	}

	// It stays unready past the progress deadline.
	h.clock.Step(29 * time.Minute)
	if res := h.reconcile(md); res != (ctrl.Result{}) {
		t.Errorf("result = %+v, want no requeue after rollback", res)
	}
	got := h.get(md)
	if got.Status.StableRevision != stable || got.Status.CanaryRevision != "" || got.Status.RolledBackRevision != canary {
		t.Fatalf("status after rollback = %+v", got.Status)
	}
	c := condition(got, servingv1alpha1.ConditionRolledBack)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "ProgressDeadlineExceeded" {
		t.Errorf("RolledBack = %+v", c)
	}
	if _, ok := h.llmisvc(md, canary); ok {
		t.Error("canary LLMInferenceService not deleted")
	}
	if _, ok := h.claimTemplate(md, canary); ok {
		t.Error("canary ResourceClaimTemplate not deleted")
	}
	route, _ = h.route(md)
	if b := backendsOf(t, route); !reflect.DeepEqual(b, []weightedBackend{{pool(md, stable), 100}}) {
		t.Errorf("backends after rollback = %v", b)
	}
	if a := got.Status.AllocatedSlices; len(a) != 1 || a[0].Revision != stable {
		t.Errorf("allocated = %+v", a)
	}
	if ev := h.events(); !hasEvent(ev, "RolledBack") {
		t.Errorf("events = %v", ev)
	}

	// The failed revision is not retried.
	h.clock.Step(time.Hour)
	h.reconcile(md)
	got = h.get(md)
	if got.Status.CanaryRevision != "" {
		t.Errorf("rolled back revision was retried: %+v", got.Status)
	}
	if !h.snapshotExists(md, canary) {
		t.Error("the snapshot of the desired revision should be kept")
	}

	// A new spec starts a new canary and clears the RolledBack condition.
	h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) { s.Runtime.Image = "vllm/vllm-openai:v0.11.1" })
	h.reconcile(md)
	got = h.get(md)
	if got.Status.CanaryRevision == "" || got.Status.CanaryRevision == canary {
		t.Errorf("new canary not started: %+v", got.Status)
	}
	if condition(got, servingv1alpha1.ConditionRolledBack) != nil {
		t.Error("RolledBack condition should be cleared by a new rollout")
	}
}

func TestCanaryAbortedWhenSpecReverted(t *testing.T) {
	md := llamaDeployment("chat", "team-a", 4)
	h := newHarness(t, md)
	stable, canary := startRollout(t, h, md)

	h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) { s.Runtime.Image = "vllm/vllm-openai:v0.10.1" })
	h.reconcile(md)
	got := h.get(md)
	if got.Status.StableRevision != stable || got.Status.CanaryRevision != "" {
		t.Errorf("status = %+v", got.Status)
	}
	if _, ok := h.llmisvc(md, canary); ok {
		t.Error("canary not removed")
	}
	if ev := h.events(); !hasEvent(ev, "CanaryAborted") {
		t.Errorf("events = %v", ev)
	}
}

func TestRecreateStrategy(t *testing.T) {
	md := llamaDeployment("batch", "team-a", 2)
	md.Spec.Rollout.Strategy = servingv1alpha1.RolloutStrategyRecreate
	h := newHarness(t, md)
	h.reconcile(md)
	old := h.get(md).Status.StableRevision

	// A larger KV cache moves the plan to 3g.40gb.
	h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) {
		s.GPUBudget.MaxMemoryGiBPerReplica = nil
		s.GPUBudget.MaxConcurrentSequences = 16
		s.Variants = s.Variants[1:2]
	})
	h.reconcile(md)
	got := h.get(md)
	if got.Status.StableRevision == old || got.Status.CanaryRevision != "" || got.Status.MIGProfile != "3g.40gb" {
		t.Fatalf("status = %+v", got.Status)
	}
	if _, ok := h.llmisvc(md, old); ok {
		t.Error("old revision not removed")
	}
	if rct, ok := h.claimTemplate(md, got.Status.StableRevision); !ok ||
		rct.Spec.Spec.Devices.Requests[0].Exactly.Selectors[0].CEL.Expression != "device.attributes['gpu.nvidia.com'].profile == '3g.40gb'" {
		t.Errorf("claim template = %+v", rct)
	}
	if ev := h.events(); !hasEvent(ev, "Recreated") {
		t.Errorf("events = %v", ev)
	}
}

func TestDriftCorrection(t *testing.T) {
	md := llamaDeployment("chat", "team-a", 4)
	h := newHarness(t, md)
	h.reconcile(md)
	rev := h.get(md).Status.StableRevision
	h.events()

	drift := func(kind string) float64 { return testutil.ToFloat64(DriftCorrections.WithLabelValues(kind)) }
	svcDrift, routeDrift, rctDrift := drift("LLMInferenceService"), drift("HTTPRoute"), drift("ResourceClaimTemplate")

	// Fields added by other controllers or webhooks are not drift.
	svc, _ := h.llmisvc(md, rev)
	if err := unstructured.SetNestedField(svc.Object, "100", "spec", "router", "scheduler", "pool", "spec", "targetPortNumber"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Update(h.ctx, svc); err != nil {
		t.Fatal(err)
	}
	svc, _ = h.llmisvc(md, rev)
	rv := svc.GetResourceVersion()
	h.reconcile(md)
	svc, _ = h.llmisvc(md, rev)
	if svc.GetResourceVersion() != rv || drift("LLMInferenceService") != svcDrift {
		t.Error("an added field must not be treated as drift")
	}

	// Someone scales the LLMInferenceService and edits its args by hand.
	if err := unstructured.SetNestedField(svc.Object, int64(9), "spec", "replicas"); err != nil {
		t.Fatal(err)
	}
	containers, _, _ := unstructured.NestedSlice(svc.Object, "spec", "template", "containers")
	containers[0].(map[string]any)["args"] = []any{"--max-model-len", "131072"}
	if err := unstructured.SetNestedSlice(svc.Object, containers, "spec", "template", "containers"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Update(h.ctx, svc); err != nil {
		t.Fatal(err)
	}
	// ... and points the route at a different backend.
	route, _ := h.route(md)
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	rules[0].(map[string]any)["backendRefs"] = []any{map[string]any{"name": "something-else", "weight": int64(100)}}
	if err := unstructured.SetNestedSlice(route.Object, rules, "spec", "rules"); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Update(h.ctx, route); err != nil {
		t.Fatal(err)
	}
	// ... and loosens the MIG selector of the claim template.
	rct, _ := h.claimTemplate(md, rev)
	rct.Spec.Spec.Devices.Requests[0].Exactly.Selectors = nil
	if err := h.c.Update(h.ctx, rct); err != nil {
		t.Fatal(err)
	}

	h.reconcile(md)

	svc, _ = h.llmisvc(md, rev)
	if n := replicasOf(t, svc); n != 4 {
		t.Errorf("replicas = %d, want 4 restored", n)
	}
	containers, _, _ = unstructured.NestedSlice(svc.Object, "spec", "template", "containers")
	args, _, _ := unstructured.NestedStringSlice(containers[0].(map[string]any), "args")
	if i := slices.Index(args, "--max-model-len"); i < 0 || args[i+1] != "8192" {
		t.Errorf("args not restored: %v", args)
	}
	route, _ = h.route(md)
	if b := backendsOf(t, route); !reflect.DeepEqual(b, []weightedBackend{{pool(md, rev), 100}}) {
		t.Errorf("route not restored: %v", b)
	}
	rct, _ = h.claimTemplate(md, rev)
	if len(rct.Spec.Spec.Devices.Requests[0].Exactly.Selectors) != 2 {
		t.Errorf("claim template not restored: %+v", rct.Spec.Spec.Devices.Requests[0].Exactly)
	}
	for kind, before := range map[string]float64{"LLMInferenceService": svcDrift, "HTTPRoute": routeDrift, "ResourceClaimTemplate": rctDrift} {
		if got := drift(kind); got != before+1 {
			t.Errorf("%s drift counter = %v, want %v", kind, got, before+1)
		}
	}
	ev := h.events()
	if n := countEvents(ev, "DriftCorrected"); n != 3 {
		t.Errorf("want 3 DriftCorrected events, got %v", ev)
	}

	// An intended change (scaling the deployment) is an update, not drift.
	h.updateSpec(md, func(s *servingv1alpha1.ModelDeploymentSpec) { s.Replicas = ptr.To[int32](3) })
	h.reconcile(md)
	svc, _ = h.llmisvc(md, rev)
	if n := replicasOf(t, svc); n != 3 {
		t.Errorf("replicas = %d, want 3", n)
	}
	if drift("LLMInferenceService") != svcDrift+1 {
		t.Error("a spec change was counted as drift")
	}

	// A deleted child is recreated.
	if err := h.c.Delete(h.ctx, svc); err != nil {
		t.Fatal(err)
	}
	h.reconcile(md)
	if _, ok := h.llmisvc(md, rev); !ok {
		t.Error("deleted LLMInferenceService not recreated")
	}
}

func countEvents(events []string, reason string) int {
	n := 0
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			n++
		}
	}
	return n
}

func TestIsSubset(t *testing.T) {
	tests := []struct {
		name       string
		want, have any
		ok         bool
	}{
		{"equal scalars", "a", "a", true},
		{"numbers of different types", int64(4), float64(4), true},
		{"different numbers", int64(4), int64(5), false},
		{"extra keys in have", map[string]any{"a": "x"}, map[string]any{"a": "x", "b": "y"}, true},
		{"missing key", map[string]any{"a": "x"}, map[string]any{"b": "y"}, false},
		{"empty map does not match a missing key", map[string]any{"s": map[string]any{}}, map[string]any{}, false},
		{"empty map matches nil", map[string]any{}, nil, true},
		{"list length differs", []any{"a"}, []any{"a", "b"}, false},
		{"list elements are subsets", []any{map[string]any{"name": "main"}}, []any{map[string]any{"name": "main", "image": "x"}}, true},
		{"type mismatch", map[string]any{}, "x", false},
	}
	for _, tt := range tests {
		if got := isSubset(tt.want, tt.have); got != tt.ok {
			t.Errorf("%s: isSubset = %v, want %v", tt.name, got, tt.ok)
		}
	}
}

func TestCanaryReplicas(t *testing.T) {
	tests := []struct{ replicas, weight, want int32 }{
		{4, 10, 1}, {4, 50, 2}, {4, 100, 4}, {10, 10, 1}, {10, 25, 3}, {1, 10, 1}, {0, 50, 0},
	}
	for _, tt := range tests {
		if got := canaryReplicas(tt.replicas, tt.weight); got != tt.want {
			t.Errorf("canaryReplicas(%d, %d) = %d, want %d", tt.replicas, tt.weight, got, tt.want)
		}
	}
}
