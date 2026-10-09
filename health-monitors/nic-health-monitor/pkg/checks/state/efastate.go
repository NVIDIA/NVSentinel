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
	"log/slog"
	"strings"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/discovery"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/statefile"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/topology"
)

const efaLinkLayer = topology.LinkLayerEFA

// netdevCarrierAttr is the /sys/class/net/<iface>/carrier attribute:
// "1" while the link has carrier, "0" when it does not. Reading it on an
// administratively-down interface fails with EINVAL, which the snapshot
// treats as "no signal" (operstate already reports such an interface as
// down).
const netdevCarrierAttr = "carrier"

// EFAStateCheck monitors AWS Elastic Fabric Adapter link state.
//
// The efa kernel driver registers with the RDMA core but hard-codes its
// port attributes: /sys/class/infiniband/rdmap*/ports/1/state always
// reads ACTIVE and phys_state always reads LinkUp, whatever the adapter
// is doing. The observable link signals are therefore the associated
// network interface (operstate/carrier under
// /sys/class/infiniband/<dev>/device/net/<iface>/, present on
// ENA+EFA dual-function adapters) and the device's presence in sysfs
// (an adapter the driver failed to reset disappears). The check folds
// the netdev signal into the port snapshot so the shared
// transition/latch/persistence pipeline judges an EFA port exactly as it
// judges a RoCE port whose driver reported DOWN.
//
// EFA-only adapters expose no netdev; for them this check still detects
// device and port disappearance and the EFA degradation check covers
// counter-based failures.
type EFAStateCheck struct {
	netdevStateCheck
}

var _ checks.TransactionalCheck = (*EFAStateCheck)(nil)

// efaFlavor owns ports tagged with the EFA link layer and derives their
// health from the adapter's netdev.
var efaFlavor = netdevFlavor{
	checkName:    checks.EFAStateCheckName,
	linkLayer:    efaLinkLayer,
	label:        "EFA",
	isTargetPort: discovery.IsEFAPort,
	snapshot:     efaPortSnapshot,
}

// NewEFAStateCheck wires the dependencies used by the EFA state check.
// It shares the persistent state file with the sibling checks (entries
// tagged with the EFA link layer) and follows the same first-poll and
// baseline contract as InfiniBandStateCheck.
func NewEFAStateCheck(
	nodeName string,
	reader sysfs.Reader,
	cfg *config.Config,
	classifier *topology.Classifier,
	processingStrategy pb.ProcessingStrategy,
	stateManager *statefile.Manager,
	bootIDChanged bool,
) *EFAStateCheck {
	c := &EFAStateCheck{}
	c.init(efaFlavor, nodeName, reader, cfg, classifier, processingStrategy, stateManager, bootIDChanged)

	return c
}

// efaPortSnapshot starts from the driver's (constant) port attributes
// and overrides them with the netdev link signal when one is available:
// a down operstate or a lost carrier marks the port DOWN/LinkDown, which
// the shared pipeline reports as fatal exactly like a RoCE port DOWN.
func efaPortSnapshot(c *netdevStateCheck, dev discovery.IBDevice, p discovery.IBPort) portSnapshot {
	snap := sysfsPortSnapshot(c, dev, p)

	if dev.NetDev == "" {
		return snap
	}

	linkDown := false

	if oper, err := c.reader.ReadNetOperState(dev.NetDev); err == nil {
		if netOperStateIsDown(oper) {
			linkDown = true
		}
	} else {
		slog.Debug("EFA netdev operstate unreadable; relying on carrier and sysfs port state",
			"device", dev.Name, "netdev", dev.NetDev, "error", err)
	}

	if carrier, err := c.reader.ReadNetAttribute(dev.NetDev, netdevCarrierAttr); err == nil && carrier == 0 {
		linkDown = true
	}

	if linkDown {
		snap.State = checks.IBStateDown
		snap.PhysicalState = checks.EFAPhysLinkDown
	}

	return snap
}

// netOperStateIsDown interprets the RFC 2863 operstate strings the
// kernel writes to /sys/class/net/<iface>/operstate. "up" is healthy;
// "unknown" is treated as healthy too because interfaces whose driver
// does not report carrier state read "unknown" while forwarding traffic
// normally. Anything else is positive evidence that the link is not
// operational.
func netOperStateIsDown(operstate string) bool {
	switch strings.ToLower(strings.TrimSpace(operstate)) {
	case "up", "unknown", "":
		return false
	default:
		return true
	}
}
