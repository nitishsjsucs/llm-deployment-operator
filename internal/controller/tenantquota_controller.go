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

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/quota"
)

// TenantQuotaReconciler keeps TenantQuota status in line with the slices the
// tenant's ModelDeployments hold.
type TenantQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=serving.llmops.dev,resources=tenantquotas,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.llmops.dev,resources=tenantquotas/status,verbs=get;update;patch

// Reconcile recomputes the usage of one TenantQuota.
func (r *TenantQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	tq := &servingv1alpha1.TenantQuota{}
	if err := r.Get(ctx, req.NamespacedName, tq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var mds servingv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &mds, client.MatchingFields{TenantField: tq.Spec.Tenant}); err != nil {
		return ctrl.Result{}, err
	}
	var used quota.Usage
	for i := range mds.Items {
		u, err := usageFromAllocations(mds.Items[i].Status.AllocatedSlices)
		if err != nil {
			return ctrl.Result{}, err
		}
		used = used.Plus(u)
	}
	status := servingv1alpha1.TenantQuotaStatus{
		ObservedGeneration: tq.Generation,
		UsedGPUMemoryGiB:   used.MemoryGiB,
		Deployments:        int32(len(mds.Items)),
	}
	for _, p := range used.Profiles() {
		status.UsedSlices = append(status.UsedSlices, servingv1alpha1.SliceCount{Profile: p, Count: int32(used.Slices[p])})
	}
	if equality.Semantic.DeepEqual(tq.Status, status) {
		return ctrl.Result{}, nil
	}
	tq.Status = status
	return ctrl.Result{}, r.Status().Update(ctx, tq)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TenantQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.TenantQuota{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&servingv1alpha1.ModelDeployment{}, handler.EnqueueRequestsFromMapFunc(r.quotasForDeployment)).
		Named("tenantquota").
		Complete(r)
}

func (r *TenantQuotaReconciler) quotasForDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	md := obj.(*servingv1alpha1.ModelDeployment)
	return quotaRequests(ctx, r.Client, md.Spec.Tenant)
}

func quotaRequests(ctx context.Context, c client.Client, tenant string) []reconcile.Request {
	var quotas servingv1alpha1.TenantQuotaList
	if err := c.List(ctx, &quotas, client.MatchingFields{TenantField: tenant}); err != nil {
		logf.FromContext(ctx).Error(err, "listing TenantQuotas", "tenant", tenant)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(quotas.Items))
	for _, q := range quotas.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: q.Name}})
	}
	return reqs
}

// usageFromAllocations sums the slices recorded in a ModelDeployment status.
func usageFromAllocations(allocs []servingv1alpha1.SliceAllocation) (quota.Usage, error) {
	var u quota.Usage
	for _, a := range allocs {
		if err := u.Add(a.Profile, int64(a.Count)); err != nil {
			return quota.Usage{}, err
		}
	}
	return u, nil
}

// quotaLimits converts TenantQuotas to quota limits.
func quotaLimits(quotas []servingv1alpha1.TenantQuota) []quota.Limits {
	out := make([]quota.Limits, 0, len(quotas))
	for _, q := range quotas {
		l := quota.Limits{Name: q.Name, MaxMemoryGiB: q.Spec.MaxGPUMemoryGiB, Slices: map[string]int64{}}
		for _, s := range q.Spec.MIGSlices {
			l.Slices[s.Profile] = int64(s.Max)
		}
		out = append(out, l)
	}
	return out
}
