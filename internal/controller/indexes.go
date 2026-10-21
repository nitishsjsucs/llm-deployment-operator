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

	"sigs.k8s.io/controller-runtime/pkg/client"

	servingv1alpha1 "github.com/nitishsjsucs/llm-deployment-operator/api/v1alpha1"
)

// TenantField is the field index on spec.tenant of ModelDeployments and TenantQuotas.
const TenantField = "spec.tenant"

// DeploymentTenant extracts the index value for ModelDeployments.
func DeploymentTenant(obj client.Object) []string {
	return []string{obj.(*servingv1alpha1.ModelDeployment).Spec.Tenant}
}

// QuotaTenant extracts the index value for TenantQuotas.
func QuotaTenant(obj client.Object) []string {
	return []string{obj.(*servingv1alpha1.TenantQuota).Spec.Tenant}
}

// SetupIndexes registers the field indexes the reconcilers query.
func SetupIndexes(ctx context.Context, indexer client.FieldIndexer) error {
	if err := indexer.IndexField(ctx, &servingv1alpha1.ModelDeployment{}, TenantField, DeploymentTenant); err != nil {
		return err
	}
	return indexer.IndexField(ctx, &servingv1alpha1.TenantQuota{}, TenantField, QuotaTenant)
}
