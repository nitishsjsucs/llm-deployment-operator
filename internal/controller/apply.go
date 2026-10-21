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
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
)

// FieldOwner is the field manager name used for writes.
const FieldOwner = "llm-deployment-operator"

// Child objects are reconciled with get, compare and update rather than
// server-side apply: the comparison treats the rendered object as the set of
// fields the operator owns, so fields added by other controllers or by
// defaulting webhooks are left alone, while any change to an owned field is
// reverted. A change is counted as drift when the live object still carries
// the spec-hash annotation of the desired object, i.e. the operator did not
// intend to change it.

// applyMutable creates desired, or updates the live object when an owned
// field differs from desired.
func (r *ModelDeploymentReconciler) applyMutable(ctx context.Context, md *servingv1alpha1.ModelDeployment, desired *unstructured.Unstructured) error {
	if err := controllerutil.SetControllerReference(md, desired, r.Scheme); err != nil {
		return err
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(desired.GroupVersionKind())
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), live); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired, client.FieldOwner(FieldOwner))
	} else if err != nil {
		return err
	}
	if inSync(md, desired.Object, live) {
		return nil
	}
	drifted := sameSpecHash(desired, live)
	live.Object["spec"] = runtime.DeepCopyJSONValue(desired.Object["spec"])
	mergeMetadata(desired, live)
	if err := controllerutil.SetControllerReference(md, live, r.Scheme); err != nil {
		return err
	}
	if err := r.Update(ctx, live, client.FieldOwner(FieldOwner)); err != nil {
		return err
	}
	if drifted {
		r.recordDrift(md, desired.GetKind(), desired.GetName())
	}
	return nil
}

// applyImmutable creates desired, or deletes and recreates the live object
// when an owned field differs. It is used for objects whose spec cannot be
// updated, such as ResourceClaimTemplates.
func (r *ModelDeploymentReconciler) applyImmutable(ctx context.Context, md *servingv1alpha1.ModelDeployment, desired client.Object) error {
	if err := controllerutil.SetControllerReference(md, desired, r.Scheme); err != nil {
		return err
	}
	gvk, err := apiutil.GVKForObject(desired, r.Scheme)
	if err != nil {
		return err
	}
	obj, err := r.Scheme.New(gvk)
	if err != nil {
		return err
	}
	live := obj.(client.Object)
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), live); apierrors.IsNotFound(err) {
		return r.Create(ctx, desired, client.FieldOwner(FieldOwner))
	} else if err != nil {
		return err
	}
	want, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return err
	}
	if inSync(md, want, live) {
		return nil
	}
	drifted := sameSpecHash(desired, live)
	uid := live.GetUID()
	if err := r.Delete(ctx, live, client.Preconditions{UID: &uid}); client.IgnoreNotFound(err) != nil {
		return err
	}
	if err := r.Create(ctx, desired, client.FieldOwner(FieldOwner)); err != nil {
		return err
	}
	if drifted {
		r.recordDrift(md, gvk.Kind, desired.GetName())
	}
	return nil
}

func (r *ModelDeploymentReconciler) recordDrift(md *servingv1alpha1.ModelDeployment, kind, name string) {
	DriftCorrections.WithLabelValues(kind).Inc()
	r.Recorder.Eventf(md, corev1.EventTypeWarning, "DriftCorrected", "%s %s was modified out of band; restored the desired state", kind, name)
}

// inSync reports whether live is controlled by md and contains every label,
// annotation and spec field of want.
func inSync(md *servingv1alpha1.ModelDeployment, want map[string]any, live client.Object) bool {
	if !metav1.IsControlledBy(live, md) {
		return false
	}
	have, err := toMap(live)
	if err != nil {
		return false
	}
	if !isSubset(want["spec"], have["spec"]) {
		return false
	}
	labels, _, _ := unstructured.NestedStringMap(want, "metadata", "labels")
	annotations, _, _ := unstructured.NestedStringMap(want, "metadata", "annotations")
	return containsAll(live.GetLabels(), labels) && containsAll(live.GetAnnotations(), annotations)
}

func toMap(obj client.Object) (map[string]any, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.Object, nil
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

func sameSpecHash(desired, live client.Object) bool {
	return live.GetAnnotations()[render.AnnotationSpecHash] == desired.GetAnnotations()[render.AnnotationSpecHash]
}

func mergeMetadata(desired, live client.Object) {
	live.SetLabels(merged(live.GetLabels(), desired.GetLabels()))
	live.SetAnnotations(merged(live.GetAnnotations(), desired.GetAnnotations()))
}

func merged(base, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(overrides))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func containsAll(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// isSubset reports whether every field set in want has the same value in
// have. Maps may have extra keys in have; lists must have the same length and
// are compared element by element. Numbers compare by value regardless of
// their Go type.
func isSubset(want, have any) bool {
	switch w := want.(type) {
	case map[string]any:
		h, ok := have.(map[string]any)
		if !ok && have != nil {
			return false
		}
		for k, wv := range w {
			hv, found := h[k]
			if !found && wv != nil {
				return false
			}
			if found && !isSubset(wv, hv) {
				return false
			}
		}
		return true
	case []any:
		h, ok := have.([]any)
		if !ok || len(h) != len(w) {
			return false
		}
		for i := range w {
			if !isSubset(w[i], h[i]) {
				return false
			}
		}
		return true
	}
	if wn, ok := toFloat(want); ok {
		hn, ok := toFloat(have)
		return ok && wn == hn
	}
	return reflect.DeepEqual(want, have)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
