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
package config

type Config struct {
	Policies []Policy `toml:"policies"`
}

type Policy struct {
	Name            string           `toml:"name"`
	Enabled         bool             `toml:"enabled"`
	Resource        ResourceSpec     `toml:"resource"`
	Predicate       PredicateSpec    `toml:"predicate"`
	NodeAssociation *AssociationSpec `toml:"nodeAssociation,omitempty"`
	HealthEvent     HealthEventSpec  `toml:"healthEvent"`
}

type ResourceSpec struct {
	Group   string `toml:"group"`
	Version string `toml:"version"`
	Kind    string `toml:"kind"`
}

type PredicateSpec struct {
	Expression string `toml:"expression"`
}

type AssociationSpec struct {
	Expression string `toml:"expression"`
}

type HealthEventSpec struct {
	ComponentClass    string   `toml:"componentClass"`
	IsFatal           bool     `toml:"isFatal"`
	Message           string   `toml:"message"`
	RecommendedAction string   `toml:"recommendedAction"`
	ErrorCode         []string `toml:"errorCode"`
}

func (r *ResourceSpec) GVK() string {
	if r.Group == "" {
		return r.Version + "/" + r.Kind
	}

	return r.Group + "/" + r.Version + "/" + r.Kind
}
