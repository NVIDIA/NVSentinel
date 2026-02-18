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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/janitor/api/v1alpha1"
	"github.com/nvidia/nvsentinel/janitor/pkg/config"
)

// nolint:cyclop // Business logic migrated from old code
func ConfigureFieldIndexers(mgr ctrl.Manager, cfg *config.Config) error {
	managerFieldIndexer := mgr.GetFieldIndexer()

	if cfg.GPUReset.Enabled {
		if err := managerFieldIndexer.IndexField(context.Background(), &corev1.Pod{}, "spec.nodeName",
			func(obj client.Object) []string {
				p, ok := obj.(*corev1.Pod)
				if !ok {
					return nil
				}
				if p.Spec.NodeName == "" {
					return nil
				}
				return []string{p.Spec.NodeName}
			}); err != nil {
			return fmt.Errorf("failed to add indexer on Pods for spec.nodeName: %w", err)
		}

		if err := managerFieldIndexer.IndexField(context.Background(), &v1alpha1.GPUReset{}, "spec.nodeName",
			func(obj client.Object) []string {
				gr, ok := obj.(*v1alpha1.GPUReset)
				if !ok {
					return nil
				}
				if gr.Spec.NodeName == "" {
					return nil
				}
				return []string{gr.Spec.NodeName}
			}); err != nil {
			return fmt.Errorf("failed to add indexer on GPUResets for spec.nodeName: %w", err)
		}
	}

	return nil
}
