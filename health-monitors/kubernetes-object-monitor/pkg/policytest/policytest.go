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
// resolves against the objects the test file lists.
package policytest

import (
	"context"
	"fmt"
	"io"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

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

// Run evaluates every case against the policies, the same way the monitor's
// reconciler does: the predicate first, then the node association, falling back
// to the resource name when the association is empty.
func Run(ctx context.Context, policies []config.Policy, s *Suite) ([]Result, error) {
	objects := make([]client.Object, 0, len(s.Objects))
	for i, o := range s.Objects {
		u := &unstructured.Unstructured{Object: o}
		if u.GetAPIVersion() == "" || u.GetKind() == "" || u.GetName() == "" {
			return nil, fmt.Errorf("object %d needs apiVersion, kind and metadata.name", i)
		}

		objects = append(objects, u)
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
		results = append(results, runCase(ctx, evaluator, byName, c))
	}

	return results, nil
}

func runCase(ctx context.Context, evaluator *policy.Evaluator, byName map[string]config.Policy, c Case) Result {
	r := Result{Case: c.Name, Policy: c.Policy}
	resource := &unstructured.Unstructured{Object: c.Resource}

	if failure := checkScope(byName, c.Policy, resource); failure != "" {
		r.Failures = append(r.Failures, failure)
		return r
	}

	match, err := evaluator.EvaluatePredicate(ctx, c.Policy, resource)
	if err != nil {
		r.Failures = append(r.Failures, fmt.Sprintf("predicate: %v", err))
		return r
	}

	r.Match = match

	node, err := evaluator.EvaluateNodeAssociation(ctx, c.Policy, resource)
	if err != nil {
		r.Failures = append(r.Failures, fmt.Sprintf("node association: %v", err))
		return r
	}

	if node == "" {
		node = resource.GetName()
	}

	r.Node = node

	if match != c.Expect.Match {
		r.Failures = append(r.Failures, fmt.Sprintf("match is %v, want %v", match, c.Expect.Match))
	}

	if c.Expect.Node != nil && node != *c.Expect.Node {
		r.Failures = append(r.Failures, fmt.Sprintf("node is %q, want %q", node, *c.Expect.Node))
	}

	return r
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
	if gvk.Group != p.Resource.Group || gvk.Version != p.Resource.Version || gvk.Kind != p.Resource.Kind {
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
