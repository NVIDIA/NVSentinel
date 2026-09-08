//go:build arm64_group
// +build arm64_group

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
	"strings"
	"testing"
	"time"

	"tests/helpers"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/e2e-framework/klient"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

type nvcreMonitorContextKey int

const (
	nvcreKeyNodeName nvcreMonitorContextKey = iota

	nvcreCertNamespace  = "nvsentinel"
	nvcreCertName       = "e2e-nvcre-cert"
	nvcreCertConfigMap  = "e2e-nvcre-cert-failed-nodes"
	nvcrePassCertName   = "e2e-nvcre-cert-pass"
	nvcrePassConfigMap  = "e2e-nvcre-cert-succeeded-nodes"
	nvcreCertDomain     = "communication"
	nvcreCertVariant    = "nccl-all-reduce"
	nvcreFailureReason  = "ThresholdViolation"
	nvcreFailureMessage = "e2e: busBandwidthGBps below threshold"
	nvcreErrorCode      = nvcreCertVariant + "/" + nvcreFailureReason

	// Second category for the partial-remediation feature.
	nvcreCertVariant2   = "nccl-all-gather"
	nvcreCertConfigMap2 = "e2e-nvcre-cert-failed-nodes-2"
	nvcreErrorCode2     = nvcreCertVariant2 + "/" + nvcreFailureReason
	nvcrePass2CertName  = "e2e-nvcre-cert-pass-2"
	nvcrePass2ConfigMap = "e2e-nvcre-cert-succeeded-nodes-2"

	// The monitor sweeps every 30s in the Tilt cluster; holding for longer than
	// one sweep proves it saw the InProgress Certification and ignored it.
	nvcreInProgressHold = 45 * time.Second
)

// The Tilt cluster has the Certification CRD but no NVCRE controller, so the
// test drives the Certification through the controller's state machine itself
// (Pending, InProgress, results published, Failed/Succeeded) and checks what
// the monitor and fault-quarantine do with it.
func TestNVCRECertificationMonitor(t *testing.T) {
	feature := features.New("NVCRE Certification Monitor - failed certification taints node").
		WithLabel("suite", "nvcre-certification-monitor").
		WithLabel("component", "certification-monitoring")

	feature.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test node: %s", nodeName)

		require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcreCertName, nvcreCertConfigMap))
		require.NoError(t, helpers.ClearNVCRENodeState(ctx, client, nodeName))

		return context.WithValue(ctx, nvcreKeyNodeName, nodeName)
	})

	feature.Assess("Results published while the Certification is still InProgress do not touch the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)
			categories := nvcreFailedCategories(nodeName, nvcreCertVariant)

			// The controller's steps are driven one at a time here, unlike the
			// other certs in this file that use CreateCertification: the cert is
			// left InProgress with its results already published so the test can
			// prove the monitor waits for the terminal condition. The next
			// assessment finishes it.
			helpers.StartCertification(ctx, t, client, nvcreCertNamespace, nvcreCertName, []string{nodeName}, categories)
			helpers.CompleteCategories(ctx, t, client, nvcreCertNamespace, nvcreCertName, categories)

			t.Logf("Holding %s to confirm the monitor ignores a non-terminal Certification", nvcreInProgressHold)
			require.Never(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				_, annotated := node.Annotations[helpers.NVCRECertFailuresAnnotationKey]

				return annotated || helpers.NodeHasNVCRETaint(node) || helpers.NodeHasNVCRECondition(node)
			}, nvcreInProgressHold, helpers.WaitInterval)

			return ctx
		})

	feature.Assess("Failed Certification annotates, taints and conditions the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			helpers.FinishCertification(ctx, t, client, nvcreCertNamespace, nvcreCertName,
				nvcreFailedCategories(nodeName, nvcreCertVariant))

			t.Log("Waiting for the monitor to record the failure on the node")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				return strings.Contains(node.Annotations[helpers.NVCRECertFailuresAnnotationKey], nvcreErrorCode)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			t.Log("Waiting for fault-quarantine to taint the node without cordoning")
			helpers.AssertQuarantineState(ctx, t, client, nodeName, helpers.QuarantineAssertion{
				ExpectCordoned: false,
				ExpectTaint: &v1.Taint{
					Key:    helpers.NVCRECertFailedTaintKey,
					Value:  "true",
					Effect: v1.TaintEffectNoSchedule,
				},
				AnnotationChecks: []helpers.AnnotationCheck{
					{Key: helpers.QuarantineHealthEventAnnotationKey, Pattern: nvcreErrorCode, ShouldExist: true},
				},
			})

			helpers.WaitForNodeConditionWithCheckName(ctx, t, client, nodeName,
				helpers.NVCRECertFailedCheckName, nvcreFailureMessage, "", v1.ConditionTrue)

			t.Log("Waiting for the Certification to be stamped as processed")
			require.Eventually(t, func() bool {
				_, ok, err := helpers.GetCertificationAnnotation(ctx, client, nvcreCertNamespace, nvcreCertName,
					helpers.NVCRECertProcessedAnnotationKey)
				if err != nil {
					t.Logf("Failed to get Certification: %v", err)
					return false
				}

				return ok
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			return ctx
		})

	feature.Assess("Deleting the Certification heals the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcreCertName, nvcreCertConfigMap))

			t.Log("Waiting for annotation, taint and condition to clear")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				_, annotated := node.Annotations[helpers.NVCRECertFailuresAnnotationKey]

				return !annotated && !helpers.NodeHasNVCRETaint(node) && !helpers.NodeHasNVCRECondition(node)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			return ctx
		})

	feature.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(nvcreKeyNodeName).(string)

		require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcreCertName, nvcreCertConfigMap))
		require.NoError(t, helpers.ClearNVCRENodeState(ctx, client, nodeName))

		return ctx
	})

	// A later Succeeded Certification for the same variant is the normal way a
	// failure clears: the operator fixes the node and reruns the certification.
	recovery := features.New("NVCRE Certification Monitor - later succeeded certification clears the failure").
		WithLabel("suite", "nvcre-certification-monitor").
		WithLabel("component", "certification-monitoring")

	recovery.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test node: %s", nodeName)

		require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcreCertName, nvcreCertConfigMap))
		require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcrePassCertName, nvcrePassConfigMap))
		require.NoError(t, helpers.ClearNVCRENodeState(ctx, client, nodeName))

		return context.WithValue(ctx, nvcreKeyNodeName, nodeName)
	})

	recovery.Assess("Failed Certification holds the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			helpers.CreateFailedCertification(ctx, t, client, nvcreCertNamespace, nvcreCertName, nvcreCertConfigMap,
				nvcreCertDomain, nvcreCertVariant, []helpers.NVCREFailedNode{
					{Name: nodeName, Reason: nvcreFailureReason, Message: nvcreFailureMessage},
				})

			t.Log("Waiting for the node to be annotated and tainted")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				return strings.Contains(node.Annotations[helpers.NVCRECertFailuresAnnotationKey], nvcreErrorCode) &&
					helpers.NodeHasNVCRETaint(node) && helpers.NodeHasNVCRECondition(node)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			t.Log("Waiting for the failed Certification to be stamped as processed")
			require.Eventually(t, func() bool {
				_, ok, err := helpers.GetCertificationAnnotation(ctx, client, nvcreCertNamespace, nvcreCertName,
					helpers.NVCRECertProcessedAnnotationKey)
				if err != nil {
					t.Logf("Failed to get Certification: %v", err)
					return false
				}

				return ok
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			return ctx
		})

	recovery.Assess("Later Succeeded Certification for the same variant clears the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			helpers.CreateSucceededCertification(ctx, t, client, nvcreCertNamespace, nvcrePassCertName, nvcrePassConfigMap,
				nvcreCertDomain, nvcreCertVariant, []string{nodeName})

			t.Log("Waiting for annotation, taint and condition to clear")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				_, annotated := node.Annotations[helpers.NVCRECertFailuresAnnotationKey]

				return !annotated && !helpers.NodeHasNVCRETaint(node) && !helpers.NodeHasNVCRECondition(node)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			return ctx
		})

	recovery.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(nvcreKeyNodeName).(string)

		require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcreCertName, nvcreCertConfigMap))
		require.NoError(t, helpers.DeleteCertification(ctx, client, nvcreCertNamespace, nvcrePassCertName, nvcrePassConfigMap))
		require.NoError(t, helpers.ClearNVCRENodeState(ctx, client, nodeName))

		return ctx
	})

	// A Certification can fail more than one category on a node. A later
	// Succeeded Certification that only covers one of them clears just that
	// failure; the node stays held until every remaining variant passes.
	partial := features.New("NVCRE Certification Monitor - partial remediation by a later certification").
		WithLabel("suite", "nvcre-certification-monitor").
		WithLabel("component", "certification-monitoring")

	partial.Setup(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName, err := helpers.GetRealNodeName(ctx, client)
		require.NoError(t, err, "failed to get real node name")
		t.Logf("Using test node: %s", nodeName)

		require.NoError(t, deletePartialCertifications(ctx, client))
		require.NoError(t, helpers.ClearNVCRENodeState(ctx, client, nodeName))

		return context.WithValue(ctx, nvcreKeyNodeName, nodeName)
	})

	partial.Assess("Certification failing two categories records both on the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			helpers.CreateCertification(ctx, t, client, nvcreCertNamespace, nvcreCertName, []string{nodeName},
				nvcreFailedCategories(nodeName, nvcreCertVariant, nvcreCertVariant2))

			t.Log("Waiting for both error codes, the taint and the condition on the node")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				annotation := node.Annotations[helpers.NVCRECertFailuresAnnotationKey]

				return strings.Contains(annotation, nvcreErrorCode) && strings.Contains(annotation, nvcreErrorCode2) &&
					helpers.NodeHasNVCRETaint(node) && helpers.NodeHasNVCRECondition(node)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			t.Log("Waiting for the failed Certification to be stamped as processed")
			require.Eventually(t, func() bool {
				_, ok, err := helpers.GetCertificationAnnotation(ctx, client, nvcreCertNamespace, nvcreCertName,
					helpers.NVCRECertProcessedAnnotationKey)
				if err != nil {
					t.Logf("Failed to get Certification: %v", err)
					return false
				}

				return ok
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			return ctx
		})

	partial.Assess("Succeeded Certification for one variant clears only that failure",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			helpers.CreateSucceededCertification(ctx, t, client, nvcreCertNamespace, nvcrePass2CertName, nvcrePass2ConfigMap,
				nvcreCertDomain, nvcreCertVariant2, []string{nodeName})

			t.Logf("Waiting for %s to clear while %s stays", nvcreErrorCode2, nvcreErrorCode)
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				annotation := node.Annotations[helpers.NVCRECertFailuresAnnotationKey]

				return strings.Contains(annotation, nvcreErrorCode) && !strings.Contains(annotation, nvcreErrorCode2)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			node, err := helpers.GetNodeByName(ctx, client, nodeName)
			require.NoError(t, err)
			require.True(t, helpers.NodeHasNVCRETaint(node), "taint must stay while a failure remains")
			require.True(t, helpers.NodeHasNVCRECondition(node), "condition must stay while a failure remains")

			return ctx
		})

	partial.Assess("Succeeded Certification for the remaining variant heals the node",
		func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
			client, err := c.NewClient()
			require.NoError(t, err)

			nodeName := ctx.Value(nvcreKeyNodeName).(string)

			helpers.CreateSucceededCertification(ctx, t, client, nvcreCertNamespace, nvcrePassCertName, nvcrePassConfigMap,
				nvcreCertDomain, nvcreCertVariant, []string{nodeName})

			t.Log("Waiting for annotation, taint and condition to clear")
			require.Eventually(t, func() bool {
				node, err := helpers.GetNodeByName(ctx, client, nodeName)
				if err != nil {
					t.Logf("Failed to get node: %v", err)
					return false
				}

				_, annotated := node.Annotations[helpers.NVCRECertFailuresAnnotationKey]

				return !annotated && !helpers.NodeHasNVCRETaint(node) && !helpers.NodeHasNVCRECondition(node)
			}, helpers.EventuallyWaitTimeout, helpers.WaitInterval)

			return ctx
		})

	partial.Teardown(func(ctx context.Context, t *testing.T, c *envconf.Config) context.Context {
		client, err := c.NewClient()
		require.NoError(t, err)

		nodeName := ctx.Value(nvcreKeyNodeName).(string)

		require.NoError(t, deletePartialCertifications(ctx, client))
		require.NoError(t, helpers.ClearNVCRENodeState(ctx, client, nodeName))

		return ctx
	})

	testEnv.Test(t, feature.Feature(), recovery.Feature(), partial.Feature())
}

// nvcreFailedCategories builds one Failed category per variant, all failing
// nodeName with the shared e2e reason and message. The first variant uses the
// primary failed-nodes ConfigMap, the second the secondary one.
func nvcreFailedCategories(nodeName string, variants ...string) []helpers.NVCRECategoryResult {
	configMaps := []string{nvcreCertConfigMap, nvcreCertConfigMap2}
	out := make([]helpers.NVCRECategoryResult, 0, len(variants))

	for i, variant := range variants {
		out = append(out, helpers.NVCRECategoryResult{
			Domain:        nvcreCertDomain,
			Variant:       variant,
			ConfigMapName: configMaps[i],
			Failed: []helpers.NVCREFailedNode{
				{Name: nodeName, Reason: nvcreFailureReason, Message: nvcreFailureMessage},
			},
		})
	}

	return out
}

func deletePartialCertifications(ctx context.Context, c klient.Client) error {
	if err := helpers.DeleteCertification(ctx, c, nvcreCertNamespace, nvcreCertName,
		nvcreCertConfigMap, nvcreCertConfigMap2); err != nil {
		return err
	}

	if err := helpers.DeleteCertification(ctx, c, nvcreCertNamespace, nvcrePassCertName, nvcrePassConfigMap); err != nil {
		return err
	}

	return helpers.DeleteCertification(ctx, c, nvcreCertNamespace, nvcrePass2CertName, nvcrePass2ConfigMap)
}
