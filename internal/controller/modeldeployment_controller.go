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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/quota"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/rollout"
)

// ModelDeploymentReconciler reconciles a ModelDeployment object.
type ModelDeploymentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Clock    clock.PassiveClock
	// InferencePoolGroup is the API group of the InferencePools KServe
	// creates, used in the HTTPRoute backendRefs.
	InferencePoolGroup string
}

// +kubebuilder:rbac:groups=serving.llmops.dev,resources=modeldeployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=serving.llmops.dev,resources=modeldeployments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=serving.llmops.dev,resources=modeldeployments/finalizers,verbs=update
// +kubebuilder:rbac:groups=serving.kserve.io,resources=llminferenceservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resource.k8s.io,resources=resourceclaimtemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile plans the deployment, advances the rollout state machine, applies
// the child objects of the live revisions and records the result in status.
func (r *ModelDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	md := &servingv1alpha1.ModelDeployment{}
	if err := r.Get(ctx, req.NamespacedName, md); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !md.DeletionTimestamp.IsZero() {
		// Children are garbage collected through their owner references.
		return ctrl.Result{}, nil
	}
	before := md.Status.DeepCopy()
	result, err := r.reconcile(ctx, md)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !equality.Semantic.DeepEqual(before, &md.Status) {
		if err := r.Status().Update(ctx, md); err != nil {
			return ctrl.Result{}, err
		}
	}
	return result, nil
}

// workload is one revision to run with a replica count.
type workload struct {
	revision string
	replicas int32
}

func (r *ModelDeploymentReconciler) reconcile(ctx context.Context, md *servingv1alpha1.ModelDeployment) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	// Defaults are applied in memory only; the reconciler writes nothing but
	// the status subresource of the ModelDeployment.
	md.Spec = withDefaults(md.Spec)
	spec := md.Spec
	md.Status.ObservedGeneration = md.Generation

	plan, err := planFor(spec)
	if err != nil {
		// Running revisions are left untouched until the spec is fixed.
		md.Status.Phase = servingv1alpha1.PhaseFailed
		if setCondition(md, servingv1alpha1.ConditionPlanned, metav1.ConditionFalse, "PlanFailed", err.Error()) {
			r.Recorder.Event(md, corev1.EventTypeWarning, "PlanFailed", err.Error())
		}
		return ctrl.Result{}, nil
	}
	tmpl := templateFor(spec, plan)
	desired := tmpl.Hash()
	md.Status.SelectedVariant = tmpl.Variant.DeepCopy()
	md.Status.MIGProfile = plan.Profile.Name
	md.Status.MemoryEstimate = memoryEstimate(plan)
	md.Status.ReplicasPerGPU = int32(plan.ReplicasPerGPU)
	setCondition(md, servingv1alpha1.ConditionPlanned, metav1.ConditionTrue, "VariantSelected", planSummary(plan, spec.GPUBudget.GPUType))
	if err := r.saveRevision(ctx, md, desired, tmpl); err != nil {
		return ctrl.Result{}, fmt.Errorf("saving revision %s: %w", desired, err)
	}

	cur := stateFromStatus(md.Status)
	templates := map[string]render.Template{desired: tmpl}
	ready := map[string]bool{}
	for _, rev := range []string{cur.Stable, cur.Canary} {
		if rev == "" {
			continue
		}
		if rev != desired {
			t, found, err := r.loadRevision(ctx, md, rev)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !found {
				// Without its snapshot the revision cannot be rendered again.
				log.Info("ControllerRevision missing; dropping revision from the rollout", "revision", rev)
				cur = dropRevision(cur, rev)
				continue
			}
			templates[rev] = t
		}
		if ready[rev], err = r.revisionReady(ctx, md, rev); err != nil {
			return ctrl.Result{}, err
		}
	}

	cfg := rolloutConfig(spec)
	decision := rollout.Next(cur, cfg, rollout.Observation{Desired: desired, CanaryReady: ready[cur.Canary], Now: r.Clock.Now()})
	next := decision.Next
	workloads := workloadsFor(next, *spec.Replicas, cfg.Steps)

	quotaMsg, err := r.enforceQuota(ctx, md, workloads, templates)
	if err != nil {
		return ctrl.Result{}, err
	}
	blocked := quotaMsg != ""
	if blocked {
		// Keep what is running: transitions that add a revision or canary
		// capacity are not taken, and no revision grows beyond its current
		// allocation. Decreases are still applied.
		if addsCapacity(decision.Action) {
			next = cur
		}
		workloads = capToAllocations(workloadsFor(next, *spec.Replicas, cfg.Steps), md.Status.AllocatedSlices)
	}

	weight := rollout.Weight(next, cfg.Steps, ready[next.Canary])
	if err := r.applyChildren(ctx, md, next, workloads, templates, weight, desired); err != nil {
		return ctrl.Result{}, err
	}

	r.updateStatus(md, cfg, next, workloads, templates, weight, ready[next.Stable], decision, quotaMsg)
	if !blocked {
		recordTransition(r.Recorder, md, decision)
	}
	return ctrl.Result{RequeueAfter: decision.RequeueAfter}, nil
}

// applyChildren applies the objects of the workloads and the HTTPRoute, then
// prunes the objects of other revisions. The snapshot of the desired revision
// is kept even when it is not running (held by quota or rolled back).
func (r *ModelDeploymentReconciler) applyChildren(ctx context.Context, md *servingv1alpha1.ModelDeployment, next rollout.State,
	workloads []workload, templates map[string]render.Template, weight int32, desired string) error {
	keep := sets.New[string]()
	for _, w := range workloads {
		t := templates[w.revision]
		keep.Insert(w.revision)
		if err := r.applyImmutable(ctx, md, render.ClaimTemplate(md, w.revision, t)); err != nil {
			return fmt.Errorf("applying ResourceClaimTemplate of revision %s: %w", w.revision, err)
		}
		if err := r.applyMutable(ctx, md, render.LLMInferenceService(md, w.revision, t, w.replicas)); err != nil {
			return fmt.Errorf("applying LLMInferenceService of revision %s: %w", w.revision, err)
		}
	}
	if keep.Has(next.Stable) {
		backends := []render.Backend{{Revision: next.Stable, Weight: 100 - weight}}
		if weight > 0 {
			backends = append(backends, render.Backend{Revision: next.Canary, Weight: weight})
		}
		if err := r.applyMutable(ctx, md, render.HTTPRoute(md, backends, r.InferencePoolGroup)); err != nil {
			return fmt.Errorf("applying HTTPRoute: %w", err)
		}
	}
	return r.prune(ctx, md, keep, keep.Clone().Insert(desired))
}

// enforceQuota returns a non-empty message when the workloads would grow the
// deployment's allocation beyond a TenantQuota of its tenant.
func (r *ModelDeploymentReconciler) enforceQuota(ctx context.Context, md *servingv1alpha1.ModelDeployment,
	workloads []workload, templates map[string]render.Template) (string, error) {
	proposed, err := usageOf(workloads, templates)
	if err != nil {
		return "", err
	}
	current, err := usageFromAllocations(md.Status.AllocatedSlices)
	if err != nil {
		return "", err
	}
	if !proposed.IncreasesOver(current) {
		return "", nil
	}
	var quotas servingv1alpha1.TenantQuotaList
	if err := r.List(ctx, &quotas, client.MatchingFields{TenantField: md.Spec.Tenant}); err != nil {
		return "", err
	}
	if len(quotas.Items) == 0 {
		return "", nil
	}
	var peers servingv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &peers, client.MatchingFields{TenantField: md.Spec.Tenant}); err != nil {
		return "", err
	}
	var others quota.Usage
	for i := range peers.Items {
		p := &peers.Items[i]
		if p.Namespace == md.Namespace && p.Name == md.Name {
			continue
		}
		u, err := usageFromAllocations(p.Status.AllocatedSlices)
		if err != nil {
			return "", err
		}
		others = others.Plus(u)
	}
	return quota.Message(quota.Check(quotaLimits(quotas.Items), others, proposed)), nil
}

func (r *ModelDeploymentReconciler) updateStatus(md *servingv1alpha1.ModelDeployment, cfg rollout.Config, next rollout.State,
	workloads []workload, templates map[string]render.Template, weight int32, stableReady bool, decision rollout.Decision, quotaMsg string) {
	st := &md.Status
	st.StableRevision = next.Stable
	st.CanaryRevision = next.Canary
	st.CanaryWeight = weight
	st.RolledBackRevision = next.RolledBack
	st.CanaryStep, st.CanaryStepStartTime = 0, nil
	if next.Canary != "" {
		st.CanaryStep = int32(next.Step)
		st.CanaryStepStartTime = &metav1.Time{Time: next.StepStarted}
	}
	st.AllocatedSlices = nil
	for _, w := range workloads {
		st.AllocatedSlices = append(st.AllocatedSlices, servingv1alpha1.SliceAllocation{
			Revision: w.revision, Profile: templates[w.revision].MIGProfile, Count: w.replicas,
		})
	}

	if quotaMsg != "" {
		if setCondition(md, servingv1alpha1.ConditionQuotaExceeded, metav1.ConditionTrue, "QuotaExceeded", quotaMsg) {
			r.Recorder.Event(md, corev1.EventTypeWarning, "QuotaExceeded", quotaMsg)
		}
	} else {
		setCondition(md, servingv1alpha1.ConditionQuotaExceeded, metav1.ConditionFalse, "WithinQuota", "allocation is within the tenant's quota")
	}

	switch {
	case next.Canary != "":
		msg := decision.Message
		if quotaMsg != "" {
			msg = fmt.Sprintf("canary %s held at its current step: tenant quota exceeded", next.Canary)
		}
		setCondition(md, servingv1alpha1.ConditionProgressing, metav1.ConditionTrue, "CanaryInProgress", msg)
	case next.RolledBack != "":
		setCondition(md, servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, "RolledBack", fmt.Sprintf("serving stable revision %s", next.Stable))
	default:
		setCondition(md, servingv1alpha1.ConditionProgressing, metav1.ConditionFalse, "Stable", fmt.Sprintf("serving stable revision %s", next.Stable))
	}

	if next.RolledBack != "" {
		setCondition(md, servingv1alpha1.ConditionRolledBack, metav1.ConditionTrue, "ProgressDeadlineExceeded",
			fmt.Sprintf("revision %s was not Ready within %s and was rolled back to %s; change the spec to roll out again",
				next.RolledBack, cfg.ProgressDeadline, next.Stable))
	} else {
		meta.RemoveStatusCondition(&st.Conditions, servingv1alpha1.ConditionRolledBack)
	}

	switch {
	case next.Stable == "":
		setCondition(md, servingv1alpha1.ConditionReady, metav1.ConditionFalse, "NotDeployed", "no revision is deployed")
	case stableReady:
		setCondition(md, servingv1alpha1.ConditionReady, metav1.ConditionTrue, "StableReady", fmt.Sprintf("LLMInferenceService %s is Ready", render.LLMInferenceServiceName(md.Name, next.Stable)))
	default:
		setCondition(md, servingv1alpha1.ConditionReady, metav1.ConditionFalse, "StableNotReady", fmt.Sprintf("waiting for LLMInferenceService %s to become Ready", render.LLMInferenceServiceName(md.Name, next.Stable)))
	}

	switch {
	case next.Canary != "":
		st.Phase = servingv1alpha1.PhaseProgressing
	case next.Stable != "" && stableReady:
		st.Phase = servingv1alpha1.PhaseReady
	default:
		st.Phase = servingv1alpha1.PhasePending
	}
}

func stateFromStatus(st servingv1alpha1.ModelDeploymentStatus) rollout.State {
	s := rollout.State{Stable: st.StableRevision, Canary: st.CanaryRevision, RolledBack: st.RolledBackRevision}
	if s.Canary != "" {
		s.Step = int(st.CanaryStep)
		if st.CanaryStepStartTime != nil {
			s.StepStarted = st.CanaryStepStartTime.Time
		}
	}
	return s
}

func dropRevision(s rollout.State, rev string) rollout.State {
	if s.Stable == rev {
		s.Stable = ""
	}
	if s.Canary == rev {
		s.Canary, s.Step, s.StepStarted = "", 0, time.Time{}
	}
	return s
}

// workloadsFor returns the revisions of s with their replica counts. The
// canary gets the share of replicas matching the weight of its step (at least
// one), so a 10% step of 4 replicas runs 1 canary replica next to 4 stable ones.
func workloadsFor(s rollout.State, replicas int32, steps []int32) []workload {
	var out []workload
	if s.Stable != "" {
		out = append(out, workload{revision: s.Stable, replicas: replicas})
	}
	if s.Canary != "" {
		out = append(out, workload{revision: s.Canary, replicas: canaryReplicas(replicas, rollout.StepWeight(steps, s.Step))})
	}
	return out
}

func canaryReplicas(replicas, weight int32) int32 {
	if replicas == 0 {
		return 0
	}
	n := (replicas*weight + 99) / 100
	return max(1, min(n, replicas))
}

// capToAllocations limits each workload to the replicas its revision holds.
func capToAllocations(ws []workload, allocs []servingv1alpha1.SliceAllocation) []workload {
	held := map[string]int32{}
	for _, a := range allocs {
		held[a.Revision] = a.Count
	}
	out := make([]workload, 0, len(ws))
	for _, w := range ws {
		if n, ok := held[w.revision]; ok {
			out = append(out, workload{revision: w.revision, replicas: min(w.replicas, n)})
		}
	}
	return out
}

func usageOf(ws []workload, templates map[string]render.Template) (quota.Usage, error) {
	var u quota.Usage
	for _, w := range ws {
		if err := u.Add(templates[w.revision].MIGProfile, int64(w.replicas)); err != nil {
			return quota.Usage{}, err
		}
	}
	return u, nil
}

// addsCapacity reports whether a transition introduces a revision or grows
// the canary, so that it must not be taken when quota is exceeded.
func addsCapacity(a rollout.Action) bool {
	switch a {
	case rollout.ActionInitialize, rollout.ActionRecreate, rollout.ActionStartCanary, rollout.ActionAdvance:
		return true
	}
	return false
}

var transitionEvents = map[rollout.Action]struct {
	eventType, reason string
}{
	rollout.ActionInitialize:  {corev1.EventTypeNormal, "Deployed"},
	rollout.ActionRecreate:    {corev1.EventTypeNormal, "Recreated"},
	rollout.ActionStartCanary: {corev1.EventTypeNormal, "CanaryStarted"},
	rollout.ActionAdvance:     {corev1.EventTypeNormal, "CanaryAdvanced"},
	rollout.ActionPromote:     {corev1.EventTypeNormal, "CanaryPromoted"},
	rollout.ActionAbort:       {corev1.EventTypeNormal, "CanaryAborted"},
	rollout.ActionRollback:    {corev1.EventTypeWarning, "RolledBack"},
}

func recordTransition(rec record.EventRecorder, md *servingv1alpha1.ModelDeployment, d rollout.Decision) {
	if e, ok := transitionEvents[d.Action]; ok {
		rec.Event(md, e.eventType, e.reason, d.Message)
	}
}

// setCondition sets a condition and reports whether it changed.
func setCondition(md *servingv1alpha1.ModelDeployment, condType string, status metav1.ConditionStatus, reason, msg string) bool {
	return meta.SetStatusCondition(&md.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: md.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ModelDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.InferencePoolGroup == "" {
		r.InferencePoolGroup = render.DefaultInferencePoolGroup
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&servingv1alpha1.ModelDeployment{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(newUnstructured(render.LLMInferenceServiceGVK.Kind, render.LLMInferenceServiceGVK.GroupVersion().String())).
		Owns(newUnstructured(render.HTTPRouteGVK.Kind, render.HTTPRouteGVK.GroupVersion().String())).
		Owns(&resourcev1.ResourceClaimTemplate{}).
		Watches(&servingv1alpha1.TenantQuota{}, handler.EnqueueRequestsFromMapFunc(r.deploymentsForQuota)).
		Named("modeldeployment").
		Complete(r)
}

// deploymentsForQuota re-queues a tenant's deployments when its quota or
// usage changes, so that deployments held back by quota retry.
func (r *ModelDeploymentReconciler) deploymentsForQuota(ctx context.Context, obj client.Object) []reconcile.Request {
	tq := obj.(*servingv1alpha1.TenantQuota)
	var mds servingv1alpha1.ModelDeploymentList
	if err := r.List(ctx, &mds, client.MatchingFields{TenantField: tq.Spec.Tenant}); err != nil {
		logf.FromContext(ctx).Error(err, "listing ModelDeployments", "tenant", tq.Spec.Tenant)
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(mds.Items))
	for _, md := range mds.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: md.Namespace, Name: md.Name}})
	}
	return reqs
}

func newUnstructured(kind, apiVersion string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	return u
}
