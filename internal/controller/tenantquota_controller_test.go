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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
)

func TestTenantQuotaStatus(t *testing.T) {
	tq := &servingv1alpha1.TenantQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Generation: 2},
		Spec:       servingv1alpha1.TenantQuotaSpec{Tenant: "team-a"},
	}
	a := llamaDeployment("a", "ns1", 4)
	a.Status.AllocatedSlices = []servingv1alpha1.SliceAllocation{
		{Revision: "r1", Profile: "1g.20gb", Count: 4},
		{Revision: "r2", Profile: "1g.20gb", Count: 1},
	}
	b := llamaDeployment("b", "ns2", 2)
	b.Status.AllocatedSlices = []servingv1alpha1.SliceAllocation{{Revision: "r3", Profile: "3g.40gb", Count: 2}}
	other := llamaDeployment("c", "ns3", 2)
	other.Spec.Tenant = "team-b"
	other.Status.AllocatedSlices = []servingv1alpha1.SliceAllocation{{Revision: "r4", Profile: "7g.80gb", Count: 2}}

	h := newHarness(t, tq, a, b, other)
	r := &TenantQuotaReconciler{Client: h.c, Scheme: h.r.Scheme}
	if _, err := r.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(tq)}); err != nil {
		t.Fatal(err)
	}
	got := &servingv1alpha1.TenantQuota{}
	if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(tq), got); err != nil {
		t.Fatal(err)
	}
	want := servingv1alpha1.TenantQuotaStatus{
		ObservedGeneration: 2,
		UsedSlices:         []servingv1alpha1.SliceCount{{Profile: "1g.20gb", Count: 5}, {Profile: "3g.40gb", Count: 2}},
		UsedGPUMemoryGiB:   5*20 + 2*40,
		Deployments:        2,
	}
	if !reflect.DeepEqual(got.Status, want) {
		t.Errorf("status = %+v, want %+v", got.Status, want)
	}

	if reqs := r.quotasForDeployment(h.ctx, a); len(reqs) != 1 || reqs[0].Name != "team-a" {
		t.Errorf("deployment change should requeue the tenant's quota, got %v", reqs)
	}
	if reqs := r.quotasForDeployment(h.ctx, other); len(reqs) != 0 {
		t.Errorf("tenant without quota should requeue nothing, got %v", reqs)
	}
}
