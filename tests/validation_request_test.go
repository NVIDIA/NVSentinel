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
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestValidationController(t *testing.T) {
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	vrName := "e2e-vr-" + suffix

	feature := features.New("TestValidationController").
		WithLabel("suite", "validation-controller")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node")
		t.Logf("Selected node for validation-controller test: %s", nodeName)

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return err
			}

			node.Spec.Unschedulable = true

			return client.Resources().Update(ctx, node)
		})
		require.NoError(t, err, "failed to cordon node")
		t.Logf("Node %s cordoned", nodeName)

		return context.WithValue(ctx, keyNodeName, nodeName)
	})

	feature.Assess("ValidationRequest is created and reaches Running", func(ctx context.Context, t *testing.T,
		c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		_, err = helpers.CreateValidationRequest(ctx, client, vrName, []string{nodeName}, []string{"e2e-smoke-test"})
		require.NoError(t, err, "ValidationRequest should be created successfully")

		helpers.WaitForValidationRequestPhase(ctx, t, client, vrName, "Running")

		return ctx
	})

	feature.Assess("Node has active-validation-request and validation-session annotations", func(ctx context.Context,
		t *testing.T, c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err, "failed to get node")

		require.Equal(t, vrName, node.Annotations[helpers.AnnotationActiveValidationRequest],
			"node should have active-validation-request annotation set to the ValidationRequest name")
		require.NotEmpty(t, node.Annotations[helpers.AnnotationValidationSession],
			"node should have a non-empty validation-session annotation")

		return ctx
	})

	feature.Assess("ValidationRequest reaches Succeeded", func(ctx context.Context, t *testing.T,
		c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForValidationRequestPhase(ctx, t, client, vrName, "Succeeded")

		return ctx
	})

	feature.Assess("Node is uncordoned and annotations are removed", func(ctx context.Context, t *testing.T,
		c *envconf.Config) context.Context {
		nodeName := ctx.Value(keyNodeName).(string)

		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		helpers.WaitForNodesCordonState(ctx, t, client, []string{nodeName}, false)

		node, err := helpers.GetNodeByName(ctx, client, nodeName)
		require.NoError(t, err, "failed to get node")

		require.Empty(t, node.Annotations[helpers.AnnotationActiveValidationRequest],
			"node should not have active-validation-request annotation once the request succeeds")
		require.Empty(t, node.Annotations[helpers.AnnotationValidationSession],
			"node should not have validation-session annotation once the request succeeds")

		return ctx
	})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err, "failed to create kubernetes client")

		nodeName := ctx.Value(keyNodeName).(string)

		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			if err != nil {
				return err
			}

			if !node.Spec.Unschedulable {
				return nil
			}

			node.Spec.Unschedulable = false

			return client.Resources().Update(ctx, node)
		})
		require.NoError(t, err, "failed to uncordon node")

		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(helpers.ValidationRequestGVK)
		target.SetName(vrName)

		err = helpers.DeleteCR(ctx, t, client, target, true)
		require.NoError(t, err, "failed to delete ValidationRequest")

		return ctx
	})

	testEnv.Test(t, feature.Feature())
}
