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

package ttl

import (
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Setup registers a TTL Reconciler for type T with the given manager.
//
// Each call installs a dedicated controller watching type T; multiple Setup
// calls are independent. The controller name must be unique per manager.
//
// Example:
//
//	ttl.Setup[*v1alpha1.RebootNode](mgr, "rebootnode-ttl",
//	    ttl.WithDefaultTTL(14*24*time.Hour),
//	    ttl.WithMetrics(metrics.GlobalMetrics.IncTTLDeletion))
func Setup[T client.Object](mgr ctrl.Manager, name string, opts ...Option[T]) error {
	r := NewReconciler[T](mgr.GetClient(), opts...)
	obj := newZeroRef[T]()

	if err := ctrl.NewControllerManagedBy(mgr).
		Named(name).
		For(obj).
		Complete(r); err != nil {
		return fmt.Errorf("build ttl controller %q: %w", name, err)
	}

	return nil
}
