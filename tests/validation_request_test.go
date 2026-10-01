//go:build amd64_group
// +build amd64_group

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

package tests

import (
	"context"
	"fmt"
	"testing"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type validationContextKey string

const (
	keyVRName    validationContextKey = "vrName"
	keyNodeNames validationContextKey = "nodeNames"
)

var (
	nvcreVariants                   = []string{"nccl-loopback", "nccl-loopback-nvswitch", "nccl-all-reduce"}
	nvcreTaintKeys                  = []string{"node.kubernetes.io/unschedulable", "kubernetes.io/arch", "nvidia.com/gpu"}
	nvcreVariantBandwidthThresholds = map[string]string{
		"nccl-loopback":          "value >= 150",
		"nccl-loopback-nvswitch": "value >= 580",
		"nccl-all-reduce":        "value >= 700",
	}
)

func TestValidationController(t *testing.T) {
	const newNodeValidationTestLabel = "nvsentinel.dgxc.nvidia.com/new-node-validation-test"

	feature := features.New("TestValidationController").
		WithLabel("suite", "validation-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeNames, err := helpers.GetRealNodeNames(ctx, client, 2)
		require.NoError(t, err, "failed to get 2 real nodes")
		t.Logf("Selected nodes for validation-controller test: %v", nodeNames)

		for _, nodeName := range nodeNames {
			err = helpers.SetNodeCordon(ctx, client, nodeName, true)
			require.NoError(t, err, "failed to cordon node %s", nodeName)

			err = helpers.SetNodeLabel(ctx, client, nodeName, newNodeValidationTestLabel, "true")
			require.NoError(t, err, "failed to label node %s as targeted for new-node-validation test", nodeName)
		}
		t.Logf("Labeled nodes %v as %s=true to trigger newNodeValidation", nodeNames, newNodeValidationTestLabel)

		return context.WithValue(ctx, keyNodeNames, nodeNames)
	})

	feature.Assess("ValidationRequest is automatically created for the new nodes",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			nodeNames := ctx.Value(keyNodeNames).([]string)

			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			vr := helpers.WaitForValidationRequestForNodes(ctx, t, client, nodeNames)
			require.NotNil(t, vr, "a ValidationRequest should be automatically created for the new nodes")

			specNodes, _, err := unstructured.NestedSlice(vr.Object, "spec", "nodes")
			require.NoError(t, err, "failed to read ValidationRequest spec.nodes")

			foundNodeNames := make([]string, 0, len(specNodes))
			for _, n := range specNodes {
				nodeEntry, ok := n.(map[string]any)
				require.True(t, ok, "spec.nodes entry should be a map")
				foundNodeNames = append(foundNodeNames, fmt.Sprintf("%v", nodeEntry["name"]))
			}

			require.ElementsMatch(t, nodeNames, foundNodeNames,
				"ValidationRequest spec.nodes should name exactly the 2 new nodes, and no others")

			for _, nodeName := range nodeNames {
				helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName, "NewNodeValidationRequested", "", "",
					corev1.ConditionTrue)
			}

			helpers.WaitForValidationRequestPhase(ctx, t, client, vr.GetName(), "Running")

			return context.WithValue(ctx, keyVRName, vr.GetName())
		})

	feature.Assess("Nodes have active-validation-request and validation-session annotations", func(ctx context.Context,
		t *testing.T, c *envconf.Config) context.Context {
		nodeNames := ctx.Value(keyNodeNames).([]string)
		vrName := ctx.Value(keyVRName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		for _, nodeName := range nodeNames {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err, "failed to get node %s", nodeName)

			require.Equal(t, vrName, node.Annotations[helpers.AnnotationActiveValidationRequest],
				"node %s should have active-validation-request annotation set to the ValidationRequest name", nodeName)
			require.NotEmpty(t, node.Annotations[helpers.AnnotationValidationSession],
				"node %s should have a non-empty validation-session annotation", nodeName)
		}

		return ctx
	})

	feature.Assess("Certification is created for the NVCRE tests with correct node and category membership",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			nodeNames := ctx.Value(keyNodeNames).([]string)
			vrName := ctx.Value(keyVRName).(string)

			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			verifyAndCompleteCertification(ctx, t, client, vrName, nodeNames)

			return ctx
		})

	feature.Assess("ValidationRequest reaches Succeeded", func(ctx context.Context, t *testing.T,
		c *envconf.Config) context.Context {
		vrName := ctx.Value(keyVRName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForValidationRequestPhase(ctx, t, client, vrName, "Succeeded")

		return ctx
	})

	feature.Assess("ValidationRequest has 3 test groups with correct node group membership",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			nodeNames := ctx.Value(keyNodeNames).([]string)
			vrName := ctx.Value(keyVRName).(string)

			client, err := c.NewClient()
			require.NoError(t, err, "failed to create kubernetes client")

			verifyTestGroups(ctx, t, client, vrName, nodeNames)

			return ctx
		})

	feature.Assess("Nodes are uncordoned and annotations are removed", func(ctx context.Context, t *testing.T,
		c *envconf.Config) context.Context {
		nodeNames := ctx.Value(keyNodeNames).([]string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesCordonState(ctx, t, client, nodeNames, false)

		for _, nodeName := range nodeNames {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err, "failed to get node %s", nodeName)

			require.Empty(t, node.Annotations[helpers.AnnotationActiveValidationRequest],
				"node %s should not have active-validation-request annotation once the request succeeds", nodeName)
			require.Empty(t, node.Annotations[helpers.AnnotationValidationSession],
				"node %s should not have validation-session annotation once the request succeeds", nodeName)
		}

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeNames := ctx.Value(keyNodeNames).([]string)

		for _, nodeName := range nodeNames {
			err = helpers.SetNodeCordon(ctx, client, nodeName, false)
			require.NoError(t, err, "failed to uncordon node %s", nodeName)

			err = helpers.RemoveNodeLabel(ctx, client, nodeName, newNodeValidationTestLabel)
			require.NoError(t, err, "failed to remove new-node-validation-test label from node %s", nodeName)

			helpers.SetNodeConditionStatus(ctx, t, client, nodeName, "NewNodeValidationRequested", "", true)
		}

		vrName := ctx.Value(keyVRName).(string)

		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(helpers.ValidationRequestGVK)
		target.SetName(vrName)

		err = helpers.DeleteCR(ctx, t, client, target, true)
		require.NoError(t, err, "failed to delete ValidationRequest")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}

func verifyAndCompleteCertification(ctx context.Context, t *testing.T, client klient.Client,
	vrName string, nodeNames []string) {
	t.Helper()

	certs := helpers.WaitForCertificationCount(ctx, t, client, vrName, 1)
	cert := certs[0]

	targetNodeNames, _, err := unstructured.NestedStringSlice(cert.Object, "spec", "target", "nodeNames")
	require.NoError(t, err, "failed to read Certification spec.target.nodeNames")
	require.ElementsMatch(t, nodeNames, targetNodeNames,
		"Certification should target exactly the 2 new nodes")

	taintSelectors, _, err := unstructured.NestedSlice(cert.Object, "spec", "target", "taintSelectors")
	require.NoError(t, err, "failed to read Certification spec.target.taintSelectors")
	require.Len(t, taintSelectors, 3, "expected the standard cordon taintSelector plus schedulingGate.taints")

	taintSelectorKeys := make([]string, 0, len(taintSelectors))

	for _, s := range taintSelectors {
		taintSelector, ok := s.(map[string]any)
		require.True(t, ok, "taintSelector entry should be a map")
		taintSelectorKeys = append(taintSelectorKeys, fmt.Sprintf("%v", taintSelector["key"]))
	}

	require.ElementsMatch(t, nvcreTaintKeys, taintSelectorKeys,
		"taintSelectors should cover the cordon taint and all schedulingGate.taints")

	categories, _, err := unstructured.NestedSlice(cert.Object, "spec", "categories")
	require.NoError(t, err, "failed to read Certification spec.categories")
	require.Len(t, categories, len(nvcreVariants), "Certification should have one category per NVCRE test")

	variants := make([]string, 0, len(categories))

	for _, c := range categories {
		category, ok := c.(map[string]any)
		require.True(t, ok, "category entry should be a map")
		require.Equal(t, "communication", category["domain"], "all 3 NVCRE tests use the communication domain")

		variant := fmt.Sprintf("%v", category["variant"])
		variants = append(variants, variant)

		threshold, _, err := unstructured.NestedString(category, "options", "thresholds", "busBandwidthGBps")
		require.NoError(t, err, "failed to read category options.thresholds.busBandwidthGBps")
		require.Equal(t, nvcreVariantBandwidthThresholds[variant], threshold,
			"busBandwidthGBps threshold should match the configured bandwidthGBps for %s", variant)
	}

	require.ElementsMatch(t, nvcreVariants, variants,
		"Certification categories should match the configured NVCRE tests")

	results := make([]helpers.NVCRECategoryResult, 0, len(nvcreVariants))
	for _, variant := range nvcreVariants {
		results = append(results, helpers.NVCRECategoryResult{
			Domain:    "communication",
			Variant:   variant,
			Succeeded: nodeNames,
		})
	}

	helpers.FinishCertification(ctx, t, client, helpers.NVSentinelNamespace, cert.GetName(), results)
}

func verifyTestGroups(ctx context.Context, t *testing.T, client klient.Client, vrName string, nodeNames []string) {
	t.Helper()

	vr := &unstructured.Unstructured{}
	vr.SetGroupVersionKind(helpers.ValidationRequestGVK)
	require.NoError(t, client.Resources().Get(ctx, vrName, "", vr), "failed to get ValidationRequest")

	testGroups, _, err := unstructured.NestedSlice(vr.Object, "status", "testGroups")
	require.NoError(t, err, "failed to read ValidationRequest status.testGroups")
	require.Len(t, testGroups, 3, "expected 3 test groups")

	var smokeTestNodes []string

	var nvcreGroupCount int

	for _, g := range testGroups {
		group, ok := g.(map[string]any)
		require.True(t, ok, "test group entry should be a map")

		provider, _, _ := unstructured.NestedString(group, "provider")
		tests, _, _ := unstructured.NestedStringSlice(group, "tests")
		nodes, _, _ := unstructured.NestedStringSlice(group, "nodes")

		switch provider {
		case "k8s-job-provider":
			require.Equal(t, []string{"e2e-smoke-test"}, tests, "k8s-job-provider group should only run e2e-smoke-test")
			require.Len(t, nodes, 1, "e2e-smoke-test does not support node batching, expected 1 node per group")
			smokeTestNodes = append(smokeTestNodes, nodes...)
		case "nvcre-provider":
			nvcreGroupCount++
			require.ElementsMatch(t, nvcreVariants, tests, "nvcre-provider group should batch all 3 NVCRE tests together")
			require.ElementsMatch(t, nodeNames, nodes, "nvcre-provider group should batch both new nodes together")
		default:
			t.Fatalf("unexpected test group provider %q", provider)
		}
	}

	require.Equal(t, 1, nvcreGroupCount, "expected exactly 1 nvcre-provider test group")
	require.ElementsMatch(t, nodeNames, smokeTestNodes, "the 2 e2e-smoke-test groups together should cover both new nodes")
}
