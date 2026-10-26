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

package state

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// newMultiPlaneStateCheck models a compute device and its four-plane SMI
// alias on the same PCI function. Only mlx5_0 appears in the topology matrix.
func newMultiPlaneStateCheck(
	t *testing.T, layer string, reverse bool, reportAfter time.Duration,
) (checks.TransactionalCheck, *stubNode, *fakeClock) {
	t.Helper()

	node := singletonDownNode(layer).addIB("smi0", &stubDevice{
		pciAddress: "0000:03:00.0", numaNode: 0,
		ports: map[int]stubPort{
			1: downPort(layer), 2: downPort(layer), 3: downPort(layer), 4: downPort(layer),
		},
	})
	reader := node.reader()
	listDirs := reader.ListDirsFunc
	reader.ListDirsFunc = func(path string) ([]string, error) {
		names, err := listDirs(path)
		if path == reader.IBBase {
			slices.Sort(names)
			if reverse {
				slices.Reverse(names)
			}
		}

		return names, err
	}
	classifier := buildClassifier(t, reader, []string{"0000:0f:00.0"}, map[string][]string{"mlx5_0": {"PIX"}})
	require.Equal(t, topology.RoleCompute, classifier.RoleOf("mlx5_0"))
	require.Equal(t, topology.RoleStorage, classifier.RoleOf("smi0"), "the alias must have no topology evidence")
	cfg := &config.Config{StateCheck: config.StateCheckConfig{ReportSuppressedAfter: reportAfter}}
	clock := &fakeClock{now: suppressedStart}
	mgr := freshStateManager(t)

	if layer == ibLinkLayer {
		check := NewInfiniBandStateCheck("multi-plane", reader, cfg,
			classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
		check.now = clock.Now

		return check, node, clock
	}

	check := NewEthernetStateCheck("multi-plane", reader, cfg,
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, mgr, false)
	check.now = clock.Now

	return check, node, clock
}

func TestState_SuppressedPort_MultiPlaneCard_ReportsRegardlessOfDeviceOrder(t *testing.T) {
	for _, layer := range bothLayers {
		for _, reverse := range []bool{false, true} {
			for _, reportAfter := range []time.Duration{0, testReportAfter} {
				t.Run(fmt.Sprintf("%s/reverse=%t/reportAfter=%s", layer, reverse, reportAfter), func(t *testing.T) {
					check, _, clock := newMultiPlaneStateCheck(t, layer, reverse, reportAfter)
					require.Empty(t, runPoll(t, check))
					require.Len(t, suppressionTimers(t, check), 5, "every port belongs to a card containing a compute device")

					clock.Advance(testReportAfter - time.Second)
					assert.Empty(t, runPoll(t, check))
					clock.Advance(time.Second)
					events := runPoll(t, check)
					if reportAfter == 0 {
						assert.Empty(t, events, "disabled reporting must remain silent")
					} else {
						assert.ElementsMatch(t, []string{"mlx5_0", "smi0", "smi0", "smi0", "smi0"}, unhealthyPorts(events))
						for _, event := range events {
							assert.True(t, event.IsFatal)
							assert.Equal(t, pb.RecommendedAction_REPLACE_VM, event.RecommendedAction)
						}
					}

					clock.Advance(24 * time.Hour)
					assert.Empty(t, runPoll(t, check), "committed delayed reports must not repeat")
				})
			}
		}
	}
}

func TestState_SuppressedPort_MultiPlaneCard_UnreadableComputeMemberKeepsTimers(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, node, clock := newMultiPlaneStateCheck(t, layer, false, testReportAfter)
			require.Empty(t, runPoll(t, check))
			require.Len(t, suppressionTimers(t, check), 5)

			node.ib["mlx5_0"].portsUnreadable = true
			clock.Advance(testReportAfter)
			assert.Empty(t, runPoll(t, check), "an unreadable member must hold reports for its card")
			assert.Len(t, suppressionTimers(t, check), 5, "the readable alias must not erase compute evidence")

			node.ib["mlx5_0"].portsUnreadable = false
			events := runPoll(t, check)
			assert.Len(t, unhealthyPorts(events), 5, "all ports must report at their original deadline once readable")
		})
	}
}

func TestState_SuppressedPort_MultiPlaneCard_ManagementSiblingStaysExempt(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, node, clock := newMultiPlaneStateCheck(t, layer, false, testReportAfter)
			node.addIB("management", &stubDevice{
				pciAddress: "0000:03:00.1", numaNode: 1,
				ports: map[int]stubPort{1: downPort(layer)},
			})
			assert.Empty(t, runPoll(t, check))
			assert.Empty(t, suppressionTimers(t, check))

			clock.Advance(24 * time.Hour)
			assert.Empty(t, runPoll(t, check), "a management sibling must exempt even a card with compute evidence")
		})
	}
}

func TestState_SuppressedPort_MultiPlaneCard_StorageAliasWithoutComputeMemberStaysExempt(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, node, clock := newMultiPlaneStateCheck(t, layer, false, testReportAfter)
			delete(node.ib, "mlx5_0")
			assert.Empty(t, runPoll(t, check))
			assert.Empty(t, suppressionTimers(t, check))

			clock.Advance(24 * time.Hour)
			assert.Empty(t, runPoll(t, check), "a storage-only card must remain exempt")
		})
	}
}

func TestState_SuppressedPort_MultiPlaneCard_ComputeMemberOnOtherLinkLayerQualifies(t *testing.T) {
	for _, layer := range bothLayers {
		t.Run(layer, func(t *testing.T) {
			check, node, clock := newMultiPlaneStateCheck(t, layer, false, testReportAfter)
			otherLayer := ibLinkLayer
			if layer == ibLinkLayer {
				otherLayer = ethLinkLayer
			}

			node.ib["mlx5_0"].ports[1] = downPort(otherLayer)
			require.Empty(t, runPoll(t, check))
			require.Len(t, suppressionTimers(t, check), 4)

			clock.Advance(testReportAfter)
			assert.ElementsMatch(t, []string{"smi0", "smi0", "smi0", "smi0"}, unhealthyPorts(runPoll(t, check)))
		})
	}
}
