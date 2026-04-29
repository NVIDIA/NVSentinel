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

// Package topology implements the NIC classification that lives entirely
// in the NIC Health Monitor. It consumes two inputs:
//
//   - The raw GPU↔NIC topology matrix published by the metadata collector
//     in `/var/lib/nvsentinel/gpu_metadata.json` (`nic_topology` field and
//     the `gpus[].pci_address` list).
//   - Per-GPU NUMA nodes from the metadata file (`gpus[].numa_node`).
//   - Live sysfs reads for per-NIC NUMA nodes.
//
// It then applies a three-step decision:
//
//  0. Default route: NIC carries the host's default IP route → Management.
//  1. NUMA gate: NIC NUMA ∉ gpu_numa_set → Management.
//  2. Topo matrix: any PIX/PXB → Compute; any NODE/PHB → Storage; all SYS
//     → fall through to the NUMA default (Storage if on a GPU NUMA,
//     Management otherwise).
//
// The package is a hard consumer of the metadata file: if the file is
// missing, unreadable, or does not contain the required fields, Load
// returns an error and the caller (main.go) fails to start. This matches
// the design's "no silent fallback" rule.
package topology

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/sysfs"
)

// Classifier is the NIC monitor's view of node topology. It is built once
// at startup from the metadata file + sysfs; polling checks call the
// read-only lookup methods (IsManagementNIC, RoleOf, PCICardOf).
type Classifier struct {
	reader sysfs.Reader

	// gpuNUMASet is the set of NUMA nodes that host at least one compute
	// GPU on this node. Derived from `gpus[].numa_node` in the metadata
	// file (populated by the metadata collector from nvidia-smi topo -m).
	gpuNUMASet map[int]struct{}

	// topology is the raw matrix from the metadata file, keyed by IB
	// device name. Each slice has one entry per GPU in `gpus[]` order.
	topology map[string][]string

	// defaultRouteDevice is the IB device name (e.g., "mlx5_8") whose
	// associated net interface carries the host's default IP route. If
	// non-empty, classify() treats it as Management. Resolved once at
	// startup from /proc/net/route + sysfs.
	defaultRouteDevice string

	// cachedRoles stores the classifier output for each known IB device
	// so repeated lookups do not re-read sysfs. Populated lazily on first
	// access.
	cachedRoles map[string]Role
	// cachedCards stores the PCI "bus:device" card grouping for each IB
	// device (e.g., "0000:47:00" for both "mlx5_3" and "mlx5_4" on a
	// dual-port card).
	cachedCards map[string]string
}

// LoadFromMetadata reads the metadata file and returns a Classifier. It
// fails loudly when:
//
//   - the file cannot be read (ENOENT, permissions, etc.);
//   - the JSON is malformed;
//   - the file contains no GPUs or no NIC topology entries.
//
// These are hard errors by design: running without them would silently
// over-monitor (treating management NICs as compute) and issue incorrect
// REPLACE_VM remediations.
func LoadFromMetadata(metadataPath string, reader sysfs.Reader, procNetRoutePath ...string) (*Classifier, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return nil, fmt.Errorf("read gpu metadata at %s: %w", metadataPath, err)
	}

	var meta model.GPUMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("parse gpu metadata at %s: %w", metadataPath, err)
	}

	if len(meta.GPUs) == 0 {
		return nil, fmt.Errorf("gpu metadata at %s has empty gpus[] — NIC monitor cannot classify NICs", metadataPath)
	}

	if len(meta.NICTopology) == 0 {
		return nil, fmt.Errorf("gpu metadata at %s has empty nic_topology — metadata collector must publish "+
			"the nvidia-smi topo -m matrix before the NIC monitor can start", metadataPath)
	}

	c := &Classifier{
		reader:      reader,
		gpuNUMASet:  make(map[int]struct{}),
		topology:    meta.NICTopology,
		cachedRoles: make(map[string]Role),
		cachedCards: make(map[string]string),
	}

	c.populateGPUNUMASet(meta.GPUs)

	if len(c.gpuNUMASet) == 0 {
		return nil, fmt.Errorf("gpu metadata at %s has no GPUs with valid NUMA nodes — "+
			"the NUMA gate cannot distinguish management NICs from compute NICs; "+
			"monitoring everything would risk false REPLACE_VM on management NIC failures", metadataPath)
	}

	routePath := DefaultProcNetRoutePath
	if len(procNetRoutePath) > 0 && procNetRoutePath[0] != "" {
		routePath = procNetRoutePath[0]
	}

	c.resolveDefaultRouteDevice(routePath)

	slog.Info("Loaded NIC topology classifier",
		"metadata_path", metadataPath,
		"gpu_count", len(meta.GPUs),
		"gpu_numa_nodes", formatIntSet(c.gpuNUMASet),
		"nic_topology_entries", len(c.topology),
		"default_route_device", c.defaultRouteDevice,
	)

	return c, nil
}

// populateGPUNUMASet reads each GPU's NUMA node from the metadata file
// (populated by the metadata collector from `nvidia-smi topo -m`).
// GPUs with numa_node < 0 (unknown) are skipped. The caller checks
// that the resulting set is non-empty and fails to start if not.
func (c *Classifier) populateGPUNUMASet(gpus []model.GPUInfo) {
	for _, g := range gpus {
		if g.NUMANode < 0 {
			continue
		}

		c.gpuNUMASet[g.NUMANode] = struct{}{}
	}
}

// IsManagementNIC returns true when the device should be excluded from
// monitoring entirely (management role).
func (c *Classifier) IsManagementNIC(device string) bool {
	return c.RoleOf(device) == RoleManagement
}

// RoleOf returns the classification for a device, caching the result.
func (c *Classifier) RoleOf(device string) Role {
	if role, ok := c.cachedRoles[device]; ok {
		return role
	}

	role := c.classify(device)
	c.cachedRoles[device] = role

	return role
}

// LogClassificationSummary logs a single INFO line summarising how all
// queried devices were classified. Call once after the first discovery
// cycle so every monitored device has been through RoleOf.
func (c *Classifier) LogClassificationSummary() {
	var compute, storage, management []string

	for device, role := range c.cachedRoles {
		switch role {
		case RoleCompute:
			compute = append(compute, device)
		case RoleStorage:
			storage = append(storage, device)
		case RoleManagement:
			management = append(management, device)
		}
	}

	sort.Strings(compute)
	sort.Strings(storage)
	sort.Strings(management)

	slog.Info("NIC classification summary",
		"compute", compute,
		"storage", storage,
		"management", management,
	)
}

// classify encapsulates the compute/storage/management decision. It is
// separated from RoleOf so tests can drive it directly without priming
// the cache.
func (c *Classifier) classify(device string) Role {
	if c.defaultRouteDevice != "" && device == c.defaultRouteDevice {
		return RoleManagement
	}

	numaOnGPUSocket := c.nicOnGPUNUMA(device)

	levels := c.topology[device]
	hasCompute, hasStorage, allSYS := summarizeLevels(levels)

	switch {
	case hasCompute:
		return RoleCompute
	case hasStorage:
		return c.classifyNonCompute(device, numaOnGPUSocket, false)
	case allSYS:
		return c.classifyNonCompute(device, numaOnGPUSocket, true)
	default:
		if numaOnGPUSocket {
			return RoleStorage
		}

		return RoleManagement
	}
}

// classifyNonCompute handles the shared logic for NICs that have NODE
// (storage) or all-SYS relationships. checkDPU is true only for the
// allSYS path where BlueField DPU exclusion applies.
func (c *Classifier) classifyNonCompute(device string, numaOnGPUSocket, checkDPU bool) Role {
	if !numaOnGPUSocket {
		return RoleManagement
	}

	if checkDPU && c.isBlueFieldDPU(device) {
		return RoleManagement
	}

	if c.isInfiniBandDevice(device) {
		return RoleCompute
	}

	return RoleStorage
}

// nicOnGPUNUMA returns true when the NIC's NUMA node hosts at least one
// compute GPU. Unknown NUMA (< 0 or read error) returns false (exclude).
func (c *Classifier) nicOnGPUNUMA(device string) bool {
	node, err := c.reader.ReadIBDeviceNUMANode(device)
	if err != nil {
		slog.Debug("Could not resolve NIC NUMA node, excluding from monitoring",
			"device", device, "error", err)

		return false
	}

	if node < 0 {
		slog.Debug("NIC has unknown NUMA node (-1), excluding from monitoring",
			"device", device)

		return false
	}

	_, ok := c.gpuNUMASet[node]

	return ok
}

// isBlueFieldDPU returns true if the NIC's HCA type matches a known
// BlueField DPU. Only consulted in the all-SYS branch.
func (c *Classifier) isBlueFieldDPU(device string) bool {
	hcaType, err := c.reader.ReadIBDeviceField(device, "hca_type")
	if err != nil {
		return false
	}

	for _, prefix := range blueFieldHCATypes {
		if strings.HasPrefix(strings.TrimSpace(hcaType), prefix) {
			slog.Info("Excluding BlueField DPU from monitoring",
				"device", device, "hca_type", hcaType)

			return true
		}
	}

	return false
}

// isInfiniBandDevice checks whether the NIC's port 1 link layer is
// "InfiniBand". On mixed IB+Ethernet platforms without PIX/PXB, this
// distinguishes compute fabric NICs from storage/management NICs.
func (c *Classifier) isInfiniBandDevice(device string) bool {
	ll, err := c.reader.ReadIBPortLinkLayer(device, 1)
	if err != nil {
		return false
	}

	return strings.TrimSpace(ll) == LinkLayerInfiniBand
}

// resolveDefaultRouteDevice finds the IB device backing the host's
// default route. If unavailable, defaultRouteDevice stays empty (no-op).
func (c *Classifier) resolveDefaultRouteDevice(procNetRoutePath string) {
	iface, err := defaultRouteInterface(procNetRoutePath)
	if err != nil {
		slog.Debug("Could not determine default route interface, "+
			"default-route exclusion disabled", "error", err)

		return
	}

	ibDevice := c.netIfaceToIBDevice(iface)
	if ibDevice == "" {
		slog.Debug("Default route interface has no IB device, "+
			"default-route exclusion disabled",
			"interface", iface)

		return
	}

	c.defaultRouteDevice = ibDevice

	slog.Info("Default route NIC will be excluded as management",
		"interface", iface, "ib_device", ibDevice)
}

// defaultRouteInterface parses /proc/net/route for the default route
// (destination 00000000) and returns the interface name.
func defaultRouteInterface(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}

	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Scan() // skip header line

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}

		if fields[1] == "00000000" {
			return fields[0], nil
		}
	}

	return "", fmt.Errorf("no default route found in %s", path)
}

// netIfaceToIBDevice maps a net interface to its IB device name via sysfs.
func (c *Classifier) netIfaceToIBDevice(iface string) string {
	ibDir := filepath.Join(c.reader.NetBasePath(), iface, "device", "infiniband")

	entries, err := c.reader.ListDirs(ibDir)
	if err != nil || len(entries) == 0 {
		return ""
	}

	return entries[0]
}

// PCICardOf returns the "bus:device" portion of a NIC's PCI slot (i.e.,
// the PCI address with the function digit stripped) so ports on the same
// physical card are grouped together. Falls back to the device name on
// sysfs errors so the caller still has a usable key.
func (c *Classifier) PCICardOf(device string) string {
	if card, ok := c.cachedCards[device]; ok {
		return card
	}

	addr, err := c.reader.ReadPCIAddress(device)
	if err != nil {
		c.cachedCards[device] = device
		return device
	}

	card := addr
	if idx := strings.LastIndex(addr, "."); idx > 0 {
		card = addr[:idx]
	}

	c.cachedCards[device] = card

	return card
}

// summarizeLevels categorises a NIC's GPU relationship levels.
func summarizeLevels(levels []string) (hasCompute, hasStorage, allSYS bool) {
	if len(levels) == 0 {
		return false, false, false
	}

	sysCount := 0

	for _, lv := range levels {
		switch lv {
		case LevelPIX, LevelPXB:
			hasCompute = true
		case LevelNODE, LevelPHB:
			hasStorage = true
		case LevelSYS:
			sysCount++
		case LevelSelf:
			// Ignored: "X" appears on diagonal GPU rows only and isn't
			// meaningful for NICs, but guard defensively.
		}
	}

	allSYS = !hasCompute && !hasStorage && sysCount == len(levels)

	return hasCompute, hasStorage, allSYS
}

// formatIntSet returns a sorted "[0 1 3]" representation for logs.
func formatIntSet(s map[int]struct{}) string {
	out := make([]int, 0, len(s))
	for k := range s {
		out = append(out, k)
	}

	sort.Ints(out)

	return fmt.Sprintf("%v", out)
}

// CheckCardHomogeneity scans per-role groups of cards and flags any card
// whose active-port count is below its role's mode. The returned anomaly
// map is keyed by card (PCI bus:device) for use as sticky state across
// poll cycles.
//
// Groups with fewer than two cards are skipped (nothing to compare
// against); groups whose mode is zero (every card has no active ports)
// are also skipped because the mode is not meaningful and we would
// otherwise flag every card in the group.
func (c *Classifier) CheckCardHomogeneity(
	cardActive map[string]int,
	cardTotal map[string]int,
	cardRole map[string]Role,
) (anomalies map[string]CardAnomaly) {
	anomalies = make(map[string]CardAnomaly)

	if len(cardTotal) < 2 {
		return anomalies
	}

	byRole := make(map[Role][]string)
	for card := range cardTotal {
		byRole[cardRole[card]] = append(byRole[cardRole[card]], card)
	}

	for role, cards := range byRole {
		if len(cards) < 2 {
			continue
		}

		mode := modeActiveCount(cardActive, cards)
		if mode == 0 {
			continue
		}

		for _, card := range cards {
			active := cardActive[card]
			if active < mode {
				anomalies[card] = CardAnomaly{
					Role:              role,
					ActiveSeen:        active,
					ExpectedModeCount: mode,
				}
			}
		}
	}

	return anomalies
}

// ExpectedDownCards returns the set of cards whose active port count
// matches their role's mode — DOWN ports on these cards are expected (e.g.,
// uncabled second port on a dual-port NIC) and should not be reported as
// fatal on the first poll cycle.
func (c *Classifier) ExpectedDownCards(
	cardActive map[string]int,
	cardTotal map[string]int,
	cardRole map[string]Role,
) map[string]struct{} {
	expected := make(map[string]struct{})

	if len(cardTotal) < 2 {
		return expected
	}

	byRole := make(map[Role][]string)
	for card := range cardTotal {
		byRole[cardRole[card]] = append(byRole[cardRole[card]], card)
	}

	for _, cards := range byRole {
		if len(cards) < 2 {
			continue
		}

		mode := modeActiveCount(cardActive, cards)
		if mode == 0 {
			continue
		}

		for _, card := range cards {
			if cardActive[card] >= mode {
				expected[card] = struct{}{}
			}
		}
	}

	return expected
}

// CardAnomaly describes a card whose active-port count is below the mode
// for its role group.
type CardAnomaly struct {
	Role              Role
	ActiveSeen        int
	ExpectedModeCount int
}

// modeActiveCount returns the most common active-port count among a
// group of cards, breaking ties toward the higher value so a fleet with
// mixed dual-/single-port cards conservatively expects dual-port behaviour.
func modeActiveCount(cardActive map[string]int, cards []string) int {
	freq := make(map[int]int)
	for _, card := range cards {
		freq[cardActive[card]]++
	}

	mode := 0
	modeFreq := 0

	for count, f := range freq {
		if f > modeFreq || (f == modeFreq && count > mode) {
			mode = count
			modeFreq = f
		}
	}

	return mode
}
