// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package testutils holds fixtures shared by the monitor's package tests.
package testutils

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

// NodeNotReadyPredicate matches a node whose Ready condition is False.
const NodeNotReadyPredicate = `resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False")` +
	`.size() > 0`

var (
	NodeGVK = schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	PodGVK  = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
)

// PolicyWithExpressions returns an enabled policy watching gvk cluster-wide.
// nodeAssociation is left unset when empty.
func PolicyWithExpressions(name string, gvk schema.GroupVersionKind, predicate, nodeAssociation string) config.Policy {
	p := config.Policy{
		Name:    name,
		Enabled: true,
		Resource: config.ResourceSpec{
			Group:   gvk.Group,
			Version: gvk.Version,
			Kind:    gvk.Kind,
		},
		Predicate: config.PredicateSpec{Expression: predicate},
	}

	if nodeAssociation != "" {
		p.NodeAssociation = &config.AssociationSpec{Expression: nodeAssociation}
	}

	return p
}
