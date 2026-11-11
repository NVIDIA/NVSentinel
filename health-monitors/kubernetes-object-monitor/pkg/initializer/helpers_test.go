// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package initializer

import (
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

const nodeNotReadyPredicate = `resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0`

var (
	nodeGVK = schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	podGVK  = schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
)

func policyWithExpressions(name string, gvk schema.GroupVersionKind, predicate, nodeAssociation string) config.Policy {
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
