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
	// override the processing strategy for the policy
	ProcessingStrategy string `toml:"processingStrategy"`
}

func (r *ResourceSpec) GVK() string {
	if r.Group == "" {
		return r.Version + "/" + r.Kind
	}

	return r.Group + "/" + r.Version + "/" + r.Kind
}

// ResourceInfo contains the metadata needed to identify a resource in health events.
// This is used to populate the entitiesImpacted field, which allows fault-quarantine
// to track each resource individually. The full GVK (Group, Version, Kind) is included
// to uniquely identify resource types, as the same Kind can exist in different API groups.
type ResourceInfo struct {
	Group     string
	Version   string
	Kind      string
	Namespace string
	Name      string
}

// GVK returns a string representation of the Group/Version/Kind.
func (r *ResourceInfo) GVK() string {
	if r.Group == "" {
		return r.Version + "/" + r.Kind
	}

	return r.Group + "/" + r.Version + "/" + r.Kind
}
