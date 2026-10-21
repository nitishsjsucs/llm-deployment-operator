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
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
)

var t0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// harness drives ModelDeploymentReconciler against the fake client. The
// fake client has no KServe or DRA controllers, so tests set the Ready
// condition of LLMInferenceServices themselves.
type harness struct {
	t     *testing.T
	ctx   context.Context
	c     client.Client
	r     *ModelDeploymentReconciler
	clock *clocktesting.FakeClock
	rec   *record.FakeRecorder
}

func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := servingv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&servingv1alpha1.ModelDeployment{}, &servingv1alpha1.TenantQuota{}).
		WithIndex(&servingv1alpha1.ModelDeployment{}, TenantField, DeploymentTenant).
		WithIndex(&servingv1alpha1.TenantQuota{}, TenantField, QuotaTenant).
		Build()
	clock := clocktesting.NewFakeClock(t0)
	rec := record.NewFakeRecorder(100)
	return &harness{
		t: t, ctx: context.Background(), c: c, clock: clock, rec: rec,
		r: &ModelDeploymentReconciler{
			Client: c, Scheme: scheme, Recorder: rec, Clock: clock,
			InferencePoolGroup: render.DefaultInferencePoolGroup,
		},
	}
}

// llamaDeployment is a Llama-3.1-8B deployment with fp16, AWQ and GPTQ
// variants and a 20 GiB per-replica budget.
func llamaDeployment(name, namespace string, replicas int32) *servingv1alpha1.ModelDeployment {
	return &servingv1alpha1.ModelDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: servingv1alpha1.ModelDeploymentSpec{
			Tenant: "team-a",
			Model: servingv1alpha1.ModelSpec{
				Family:     servingv1alpha1.ModelFamilyLlama,
				Name:       "meta-llama/Llama-3.1-8B-Instruct",
				CatalogRef: "llama-3.1-8b",
			},
			Variants: []servingv1alpha1.ModelVariant{
				{Quantization: servingv1alpha1.QuantizationFP16, URI: "hf://meta-llama/Llama-3.1-8B-Instruct"},
				{Quantization: servingv1alpha1.QuantizationAWQ, URI: "hf://hugging-quants/Meta-Llama-3.1-8B-Instruct-AWQ-INT4"},
				{Quantization: servingv1alpha1.QuantizationGPTQ, URI: "hf://hugging-quants/Meta-Llama-3.1-8B-Instruct-GPTQ-INT4"},
			},
			GPUBudget: servingv1alpha1.GPUBudget{GPUType: "A100-80GB", MaxMemoryGiBPerReplica: ptr.To[int32](20)},
			Replicas:  ptr.To(replicas),
			Runtime:   servingv1alpha1.RuntimeSpec{Image: "vllm/vllm-openai:v0.10.1"},
		},
	}
}

func (h *harness) reconcile(md *servingv1alpha1.ModelDeployment) ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(md)})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (h *harness) get(md *servingv1alpha1.ModelDeployment) *servingv1alpha1.ModelDeployment {
	h.t.Helper()
	out := &servingv1alpha1.ModelDeployment{}
	if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(md), out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// updateSpec changes the spec and bumps the generation, as the API server would.
func (h *harness) updateSpec(md *servingv1alpha1.ModelDeployment, mutate func(*servingv1alpha1.ModelDeploymentSpec)) {
	h.t.Helper()
	cur := h.get(md)
	mutate(&cur.Spec)
	cur.Generation++
	if err := h.c.Update(h.ctx, cur); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) object(gvk schema.GroupVersionKind, namespace, name string) (*unstructured.Unstructured, bool) {
	h.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	err := h.c.Get(h.ctx, types.NamespacedName{Namespace: namespace, Name: name}, u)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return u, true
}

func (h *harness) llmisvc(md *servingv1alpha1.ModelDeployment, rev string) (*unstructured.Unstructured, bool) {
	return h.object(render.LLMInferenceServiceGVK, md.Namespace, render.LLMInferenceServiceName(md.Name, rev))
}

func (h *harness) route(md *servingv1alpha1.ModelDeployment) (*unstructured.Unstructured, bool) {
	return h.object(render.HTTPRouteGVK, md.Namespace, render.RouteName(md.Name))
}

func (h *harness) claimTemplate(md *servingv1alpha1.ModelDeployment, rev string) (*resourcev1.ResourceClaimTemplate, bool) {
	h.t.Helper()
	rct := &resourcev1.ResourceClaimTemplate{}
	err := h.c.Get(h.ctx, types.NamespacedName{Namespace: md.Namespace, Name: render.ClaimTemplateName(md.Name, rev)}, rct)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return rct, true
}

func (h *harness) snapshotExists(md *servingv1alpha1.ModelDeployment, rev string) bool {
	h.t.Helper()
	err := h.c.Get(h.ctx, types.NamespacedName{Namespace: md.Namespace, Name: render.ControllerRevisionName(md.Name, rev)}, &appsv1.ControllerRevision{})
	if err != nil && !apierrors.IsNotFound(err) {
		h.t.Fatal(err)
	}
	return err == nil
}

// setReady sets the Ready condition of a revision's LLMInferenceService, as
// KServe would.
func (h *harness) setReady(md *servingv1alpha1.ModelDeployment, rev string, ready bool) {
	h.t.Helper()
	u, ok := h.llmisvc(md, rev)
	if !ok {
		h.t.Fatalf("LLMInferenceService of revision %s not found", rev)
	}
	status := "False"
	if ready {
		status = "True"
	}
	if err := unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "Ready", "status": status}}, "status", "conditions"); err != nil {
		h.t.Fatal(err)
	}
	if err := h.c.Update(h.ctx, u); err != nil {
		h.t.Fatal(err)
	}
}

// events drains the recorded events.
func (h *harness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func hasEvent(events []string, reason string) bool {
	for _, e := range events {
		if strings.Contains(e, " "+reason+" ") {
			return true
		}
	}
	return false
}

func condition(md *servingv1alpha1.ModelDeployment, condType string) *metav1.Condition {
	return meta.FindStatusCondition(md.Status.Conditions, condType)
}

func replicasOf(t *testing.T, u *unstructured.Unstructured) int64 {
	t.Helper()
	n, _, err := unstructured.NestedInt64(u.Object, "spec", "replicas")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

type weightedBackend struct {
	name   string
	weight int64
}

func backendsOf(t *testing.T, route *unstructured.Unstructured) []weightedBackend {
	t.Helper()
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 1 {
		t.Fatalf("want one rule, got %v", rules)
	}
	var out []weightedBackend
	for _, r := range rules[0].(map[string]any)["backendRefs"].([]any) {
		ref := r.(map[string]any)
		w, _ := toFloat(ref["weight"])
		out = append(out, weightedBackend{name: ref["name"].(string), weight: int64(w)})
	}
	return out
}

func pool(md *servingv1alpha1.ModelDeployment, rev string) string {
	return render.LLMInferenceServiceName(md.Name, rev) + render.InferencePoolSuffix
}
