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
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
	"github.com/nitishsjsucs/llm-deployment-operator/internal/render"
)

// Each revision's Template is stored in a ControllerRevision, as StatefulSets
// do, so that the stable revision can still be rendered (and its drift
// corrected) after the spec has moved on to a canary.

func (r *ModelDeploymentReconciler) saveRevision(ctx context.Context, md *servingv1alpha1.ModelDeployment, rev string, t render.Template) error {
	name := render.ControllerRevisionName(md.Name, rev)
	err := r.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: name}, &appsv1.ControllerRevision{})
	if !apierrors.IsNotFound(err) {
		return err
	}
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: md.Namespace, Labels: render.Labels(md, rev)},
		Data:       runtime.RawExtension{Raw: data},
		Revision:   md.Generation,
	}
	if err := controllerutil.SetControllerReference(md, cr, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, cr, client.FieldOwner(FieldOwner)); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// loadRevision returns the stored Template of rev; found is false when the
// ControllerRevision does not exist.
func (r *ModelDeploymentReconciler) loadRevision(ctx context.Context, md *servingv1alpha1.ModelDeployment, rev string) (t render.Template, found bool, err error) {
	cr := &appsv1.ControllerRevision{}
	err = r.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: render.ControllerRevisionName(md.Name, rev)}, cr)
	if apierrors.IsNotFound(err) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	if err := json.Unmarshal(cr.Data.Raw, &t); err != nil {
		return t, false, fmt.Errorf("decoding ControllerRevision %s: %w", cr.Name, err)
	}
	return t, true, nil
}

// revisionReady reports whether the revision's LLMInferenceService is Ready.
func (r *ModelDeploymentReconciler) revisionReady(ctx context.Context, md *servingv1alpha1.ModelDeployment, rev string) (bool, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(render.LLMInferenceServiceGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: md.Namespace, Name: render.LLMInferenceServiceName(md.Name, rev)}, u)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return render.IsReady(u), nil
}

// prune deletes the per-revision children of revisions that are not in keep,
// and ControllerRevisions that are not in keepSnapshots.
func (r *ModelDeploymentReconciler) prune(ctx context.Context, md *servingv1alpha1.ModelDeployment, keep, keepSnapshots sets.Set[string]) error {
	llmisvcs := &unstructured.UnstructuredList{}
	llmisvcs.SetGroupVersionKind(render.LLMInferenceServiceGVK.GroupVersion().WithKind(render.LLMInferenceServiceGVK.Kind + "List"))
	claims := &resourcev1.ResourceClaimTemplateList{}
	snapshots := &appsv1.ControllerRevisionList{}

	opts := []client.ListOption{client.InNamespace(md.Namespace), client.MatchingLabels{render.LabelDeployment: md.Name}}
	for _, l := range []struct {
		list client.ObjectList
		keep sets.Set[string]
	}{{llmisvcs, keep}, {claims, keep}, {snapshots, keepSnapshots}} {
		if err := r.List(ctx, l.list, opts...); err != nil {
			return err
		}
		items, err := metaItems(l.list)
		if err != nil {
			return err
		}
		for _, obj := range items {
			if !metav1.IsControlledBy(obj, md) || l.keep.Has(obj.GetLabels()[render.LabelRevision]) {
				continue
			}
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

func metaItems(list client.ObjectList) ([]client.Object, error) {
	objs, err := meta.ExtractList(list)
	if err != nil {
		return nil, err
	}
	out := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		obj, ok := o.(client.Object)
		if !ok {
			return nil, fmt.Errorf("list item %T is not a client.Object", o)
		}
		out = append(out, obj)
	}
	return out, nil
}
