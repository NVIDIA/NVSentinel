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

// Package cacheplan works out, from the enabled policies, how the monitor
// caches each kind they use: which fields it keeps, which namespaces it caches,
// and whether lookup() reads the cache or the API server. The monitor builds
// its cache from the plan, and --test-policies uses it to give each policy the
// same objects it sees in the cluster.
package cacheplan

import (
	"log/slog"
	"maps"
	"slices"

	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"

	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
)

// Entry is how the monitor caches one GVK.
type Entry struct {
	// Transform prunes each cached object to the fields the policies read, and
	// is nil when the objects are cached in full.
	Transform toolscache.TransformFunc
	// Namespaces are the namespaces the GVK is cached for, or nil when it is
	// cached cluster-wide.
	Namespaces []string
	// LookupsFromCache reports whether lookup() of the GVK reads the cache.
	// Otherwise a lookup reads the API server and gets the whole object.
	LookupsFromCache bool
}

// Build returns an entry for each GVK the enabled policies watch, or look up
// through the cache.
func Build(compiler *celenv.Environment, policies []config.Policy) map[schema.GroupVersionKind]Entry {
	namespaces := watchNamespaces(policies)
	entries := buildCacheEntries(compiler, policies)
	plan := make(map[schema.GroupVersionKind]Entry, len(entries))

	for gvk, entry := range entries {
		e := Entry{
			Transform:        entry.transform,
			Namespaces:       namespaces[gvk],
			LookupsFromCache: entry.servesLookups,
		}

		// A lookup names any namespace it likes, and reading an entry that
		// holds only some of them fails for the rest.
		if e.LookupsFromCache && e.Namespaces != nil {
			slog.Info("Reading lookup() through the API: the GVK is cached for named namespaces only",
				"gvk", gvk.String(), "namespaces", e.Namespaces)

			e.LookupsFromCache = false
		}

		plan[gvk] = e
	}

	return plan
}

// watchNamespaces returns the namespaces of each GVK that the policies watch in
// named namespaces only. One policy naming no namespace caches the GVK
// cluster-wide whatever the others ask for, since a wider cache answers their
// reads too.
func watchNamespaces(policies []config.Policy) map[schema.GroupVersionKind][]string {
	clusterWide := make(map[schema.GroupVersionKind]bool)
	named := make(map[schema.GroupVersionKind]map[string]bool)

	for _, p := range policies {
		if !p.Enabled {
			continue
		}

		gvk := policyGVK(p)
		if p.Resource.Namespace == "" {
			clusterWide[gvk] = true

			continue
		}

		if named[gvk] == nil {
			named[gvk] = make(map[string]bool)
		}

		named[gvk][p.Resource.Namespace] = true
	}

	out := make(map[schema.GroupVersionKind][]string, len(named))

	for gvk, set := range named {
		if !clusterWide[gvk] {
			out[gvk] = slices.Sorted(maps.Keys(set))
		}
	}

	return out
}

func policyGVK(p config.Policy) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   p.Resource.Group,
		Version: p.Resource.Version,
		Kind:    p.Resource.Kind,
	}
}
