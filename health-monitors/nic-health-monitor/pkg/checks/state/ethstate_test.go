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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

// TestEthState_FirstPollDownSingletonStorageIsSuppressed reproduces the
// OCI BM.GPU.H100.8 false positive: the Prime frontend NIC carries the
// default route and is excluded as Management, leaving its Aux twin
// (intentionally Disabled, no VNIC) as a singleton Storage card. With no
// peers to compare against there is no evidence the port should be up,
// so the first-poll DOWN must stay local to monitor logs — no HealthEvent.
func TestEthState_FirstPollDownSingletonStorageIsSuppressed(t *testing.T) {
	node := newStubNode().
		addIB("mlx5_0", &stubDevice{ // compute fabric NIC (IB — ignored by eth check)
			pciAddress: "0000:18:00.0", numaNode: 0,
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "InfiniBand"}},
		}).
		addIB("mlx5_2", &stubDevice{ // Prime frontend port, excluded via default route
			pciAddress: "0000:9a:00.0", numaNode: 0, netDev: "eth0",
			ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
		}).
		addIB("mlx5_11", &stubDevice{ // lone Aux frontend port, Disabled by design
			pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
			ports: map[int]stubPort{1: {state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}},
		})
	node.nets["eth0"] = "up"
	node.nets["eth1"] = "down"

	reader := node.reader()
	routePath := writeProcNetRoute(t, "eth0")
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_0": {"PIX"}, "mlx5_2": {"NODE"}, "mlx5_11": {"NODE"}},
		routePath,
	)
	require.Equal(t, topology.RoleManagement, classifier.RoleOf("mlx5_2"))
	require.Equal(t, topology.RoleStorage, classifier.RoleOf("mlx5_11"))

	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events, "singleton storage card has no peer evidence; first-poll DOWN must be suppressed")
}

// TestEthState_RuntimeActiveToDownStaysFatal guards the other side of the
// first-poll rule: a port that was observed healthy and then goes DOWN is
// a real failure and must stay fatal regardless of peer-group size.
func TestEthState_RuntimeActiveToDownStaysFatal(t *testing.T) {
	node := newStubNode().addIB("mlx5_11", &stubDevice{
		pciAddress: "0000:a0:00.1", numaNode: 0, netDev: "eth1",
		ports: map[int]stubPort{1: {state: "ACTIVE", physState: "LinkUp", linkLayer: "Ethernet"}},
	})
	node.nets["eth1"] = "up"

	reader := node.reader()
	classifier := buildClassifier(t, reader,
		[]string{"0000:0f:00.0"},
		map[string][]string{"mlx5_11": {"NODE"}},
	)

	check := NewEthernetStateCheck("node1", reader, &config.Config{},
		classifier, pb.ProcessingStrategy_EXECUTE_REMEDIATION, freshStateManager(t), false)

	// Poll 1: healthy, first-seen — no events.
	events, err := check.Run()
	require.NoError(t, err)
	assert.Empty(t, events)

	// Port drops at runtime.
	node.ib["mlx5_11"].ports[1] = stubPort{state: "DOWN", physState: "Disabled", linkLayer: "Ethernet"}
	node.nets["eth1"] = "down"

	events, err = check.Run()
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.True(t, events[0].IsFatal, "runtime ACTIVE→DOWN is a real failure and must stay fatal")
	assert.Equal(t, pb.RecommendedAction_REPLACE_VM, events[0].RecommendedAction)
}
