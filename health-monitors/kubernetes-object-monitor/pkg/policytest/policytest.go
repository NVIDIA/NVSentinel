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

// Package policytest evaluates policies against test cases offline, with no cluster.
// Each case gives a resource and the expected predicate result and node; lookup()
// resolves against the objects the test file lists. Both are pruned the way the
// monitor's cache prunes them, so a policy sees the same fields it sees in the
// cluster.
package policytest

import (
	"context"
	"fmt"
	"io"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cacheplan"
	celenv "github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/cel"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/kubernetes-object-monitor/pkg/policy"
)

// Suite is the content of a test file.
type Suite struct {
	// Objects are the cluster state that lookup() sees, as Kubernetes manifests.
	Objects []map[string]any `json:"objects"`
	Cases   []Case           `json:"cases"`
}

// Case is one resource evaluated against one policy.
type Case struct {
	Name     string         `json:"name"`
	Policy   string         `json:"policy"`
	Resource map[string]any `json:"resource"`
	Expect   Expect         `json:"expect"`
}

// Expect is the expected outcome of a case. Node is only checked when set.
type Expect struct {
	Match bool    `json:"match"`
	Node  *string `json:"node,omitempty"`
}

// Result is the outcome of one case. Failures is empty when the case passed.
type Result struct {
	Case     string
	Policy   string
	Match    bool
	Node     string
	Failures []string
}

// LoadSuite reads and validates a test file.
func LoadSuite(path string) (*Suite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read test file: %w", err)
	}

	var s Suite
	if err := yaml.UnmarshalStrict(data, &s); err != nil {
		return nil, fmt.Errorf("parse test file %s: %w", path, err)
	}

	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("test file %s has no cases", path)
	}

	seen := make(map[string]bool, len(s.Cases))

	for i, c := range s.Cases {
		switch {
		case c.Name == "":
			return nil, fmt.Errorf("case %d has no name", i)
		case seen[c.Name]:
			return nil, fmt.Errorf("duplicate case name %q", c.Name)
		case c.Policy == "":
			return nil, fmt.Errorf("case %q has no policy", c.Name)
		case c.Resource == nil:
			return nil, fmt.Errorf("case %q has no resource", c.Name)
		}

		seen[c.Name] = true
	}

	return &s, nil
}

// Run evaluates every case against the policies with policy.Evaluator.Evaluate,
// the same call the monitor's reconciler makes.
func Run(ctx context.Context, policies []config.Policy, s *Suite) ([]Result, error) {
	compiler, err := celenv.NewCompilerEnvironment()
	if err != nil {
		return nil, fmt.Errorf("create CEL compiler: %w", err)
	}

	plan := cacheplan.Build(compiler, policies)

	objects, err := clusterObjects(plan, s.Objects)
	if err != nil {
		return nil, err
	}

	reader := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithObjects(objects...).Build()

	env, err := celenv.NewEnvironment(reader)
	if err != nil {
		return nil, fmt.Errorf("create CEL environment: %w", err)
	}

	evaluator, err := policy.NewEvaluator(env, policies)
	if err != nil {
		return nil, err
	}

	byName := make(map[string]config.Policy, len(policies))
	for _, p := range policies {
		byName[p.Name] = p
	}

	results := make([]Result, 0, len(s.Cases))
	for _, c := range s.Cases {
		results = append(results, runCase(ctx, evaluator, plan, byName, c))
	}

	return results, nil
}

// clusterObjects validates the test file's objects and returns them as lookup()
// sees them in the cluster.
func clusterObjects(
	plan map[schema.GroupVersionKind]cacheplan.Entry,
	manifests []map[string]any,
) ([]client.Object, error) {
	type objectKey struct {
		apiVersion, kind, namespace, name string
	}

	objects := make([]client.Object, 0, len(manifests))
	seen := make(map[objectKey]int, len(manifests))

	for i, o := range manifests {
		u := &unstructured.Unstructured{Object: o}
		if u.GetAPIVersion() == "" || u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("object %d needs apiVersion, kind and metadata.name", i)
		}

		key := objectKey{u.GetAPIVersion(), u.GetKind(), u.GetNamespace(), u.GetName()}
		if first, ok := seen[key]; ok {
			return nil, fmt.Errorf("object %d duplicates object %d (%s %s %s/%s)",
				i, first, key.apiVersion, key.kind, key.namespace, key.name)
		}

		seen[key] = i

		cached, err := lookedUp(plan, u)
		if err != nil {
			return nil, fmt.Errorf("object %d: %w", i, err)
		}

		objects = append(objects, cached)
	}

	return objects, nil
}

func runCase(
	ctx context.Context,
	evaluator *policy.Evaluator,
	plan map[schema.GroupVersionKind]cacheplan.Entry,
	byName map[string]config.Policy,
	c Case,
) Result {
	r := Result{Case: c.Name, Policy: c.Policy}
	resource := &unstructured.Unstructured{Object: c.Resource}

	if failure := checkScope(byName, c.Policy, resource); failure != "" {
		r.Failures = append(r.Failures, failure)
		return r
	}

	resource, err := pruned(plan[resource.GroupVersionKind()], resource)
	if err != nil {
		r.Failures = append(r.Failures, err.Error())
		return r
	}

	match, node, err := evaluator.Evaluate(ctx, c.Policy, resource)
	r.Match = match
	r.Node = node

	if err != nil {
		r.Failures = append(r.Failures, err.Error())
		return r
	}

	if match != c.Expect.Match {
		r.Failures = append(r.Failures, fmt.Sprintf("match is %v, want %v", match, c.Expect.Match))
	}

	if c.Expect.Node != nil && node != *c.Expect.Node {
		r.Failures = append(r.Failures, fmt.Sprintf("node is %q, want %q", node, *c.Expect.Node))
	}

	return r
}

// lookedUp returns obj as lookup() sees it in the cluster: pruned when the
// monitor serves lookups of its GVK from the cache, whole when it reads the API.
func lookedUp(
	plan map[schema.GroupVersionKind]cacheplan.Entry,
	obj *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	entry, ok := plan[obj.GroupVersionKind()]
	if !ok || !entry.LookupsFromCache {
		return obj, nil
	}

	return pruned(entry, obj)
}

// pruned returns a copy of obj with the fields the entry's cache transform
// drops removed, or obj itself when the GVK is cached whole.
func pruned(entry cacheplan.Entry, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	if entry.Transform == nil {
		return obj, nil
	}

	out, err := entry.Transform(obj.DeepCopy())
	if err != nil {
		return nil, fmt.Errorf("prune %s %s: %w", obj.GetKind(), obj.GetName(), err)
	}

	u, ok := out.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("prune %s %s: transform returned %T", obj.GetKind(), obj.GetName(), out)
	}

	return u, nil
}

// checkScope reports why the monitor would never evaluate the resource with the
// policy, or "" when it would.
func checkScope(byName map[string]config.Policy, name string, resource *unstructured.Unstructured) string {
	p, ok := byName[name]
	if !ok {
		return fmt.Sprintf("policy %q is not in the config", name)
	}

	if !p.Enabled {
		return fmt.Sprintf("policy %q is disabled", name)
	}

	gvk := resource.GroupVersionKind()
	if gvk != p.Resource.GroupVersionKind() {
		return fmt.Sprintf("resource is %s but policy %q watches %s, so the monitor never evaluates it",
			resourceGVK(gvk.Group, gvk.Version, gvk.Kind), p.Name, p.Resource.GVK())
	}

	if !p.Resource.MatchesNamespace(resource.GetNamespace()) {
		return fmt.Sprintf("resource is in namespace %q but policy %q watches namespace %q",
			resource.GetNamespace(), p.Name, p.Resource.Namespace)
	}

	return ""
}

func resourceGVK(group, version, kind string) string {
	return (&config.ResourceSpec{Group: group, Version: version, Kind: kind}).GVK()
}

// Report writes one line per case and returns how many failed.
func Report(w io.Writer, results []Result) int {
	failed := 0

	for _, r := range results {
		status := "PASS"
		if len(r.Failures) > 0 {
			status = "FAIL"
			failed++
		}

		fmt.Fprintf(w, "%s  %s  [%s] match=%v node=%s\n", status, r.Case, r.Policy, r.Match, r.Node)

		for _, f := range r.Failures {
			fmt.Fprintf(w, "      %s\n", f)
		}
	}

	fmt.Fprintf(w, "\n%d passed, %d failed\n", len(results)-failed, failed)

	return failed
}
