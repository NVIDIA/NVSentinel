// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
	"github.com/nvidia/nvsentinel/lifecycle-manager/pkg/config"
)

const (
	annotationActiveValidationRequest = "nvsentinel.nvidia.com/active-validation-request"
	annotationValidationSession       = "nvsentinel.nvidia.com/validation-session"
)

type ValidationRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.Config
}

func (r *ValidationRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = logf.FromContext(ctx)

	// TODO: reconciliation logic

	return ctrl.Result{}, nil
}

/*
The ValidationRequestReconciler will leverage the following queuing behavior for ValidationRequests:
 1. ValidationRequest CREATE, UPDATE, and DELETE events will fire the reconciler. This is specified with the For
    configuration.
 2. Node CREATE, UPDATE, and DELETE events where the node has the validation-session annotation will fire the
    reconciler for the ValidationRequest named in the active-validation-request annotation. In practice, CREATE events
    will never fire due to the annotation not existing and DELETE events will be a no-op after detecting that the
    object is gone.
 3. TestProvider resource CREATE, UPDATE, and DELETE events which have an OwnerReference for a ValidationRequest.
*/
func (r *ValidationRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ValidationRequest{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.nodeToValidationRequest),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				_, ok := obj.GetAnnotations()[annotationValidationSession]
				return ok
			}))).
		Named("validationrequest")

	// We need to reference the dynamic types from the TestProviders in the ValidationConfiguration. Normally, you can
	// specify a static type in Owns like this: Owns(&batchv1.Job{})
	for _, provider := range r.Config.Validation.Spec.Providers {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   provider.APIGroup,
			Version: provider.Version,
			Kind:    provider.Kind,
		})

		b = b.Owns(u)
	}

	return b.Complete(r)
}

// nodeToValidationRequest maps a node event to the ValidationRequest named in its
// nvsentinel.nvidia.com/active-validation-request annotation which includes the name for its corresponding
// ValidationRequest (which is cluster-scoped). Note that this is paired with a predicate to only fire for nodes which
// have the annotation present so we should expect the annotation to always exist here.
func (r *ValidationRequestReconciler) nodeToValidationRequest(_ context.Context,
	obj client.Object) []reconcile.Request {
	name, ok := obj.GetAnnotations()[annotationActiveValidationRequest]
	if !ok || len(name) == 0 {
		return nil
	}

	return []reconcile.Request{
		{NamespacedName: types.NamespacedName{Name: name}},
	}
}
