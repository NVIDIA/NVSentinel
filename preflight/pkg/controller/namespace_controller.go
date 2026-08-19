// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const preflightNamespaceLabel = "nvsentinel.nvidia.com/preflight"

// NamespaceReconciler keeps ActiveNamespaces in sync with the set of namespaces
// that carry the preflight-enabled label. It is the source of truth that the
// pod cache transform uses to decide whether to retain full gang fields or emit
// a minimal stub.
type NamespaceReconciler struct {
	client.Client
	active *ActiveNamespaces
}

func NewNamespaceReconciler(c client.Client, active *ActiveNamespaces) *NamespaceReconciler {
	return &NamespaceReconciler{Client: c, active: active}
}

func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Namespace{}).
		Complete(r)
}

func (r *NamespaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, req.NamespacedName, &ns); err != nil {
		if errors.IsNotFound(err) {
			r.active.Remove(req.Name)

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("failed to get namespace %s: %w", req.Name, err)
	}

	if ns.DeletionTimestamp == nil && ns.Labels[preflightNamespaceLabel] == "enabled" {
		r.active.Add(ns.Name)
	} else {
		r.active.Remove(ns.Name)
	}

	return ctrl.Result{}, nil
}
