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

// Package statefile manages the NIC Health Monitor's persistent state
// file. The file is a single JSON document storing port snapshots,
// known devices, counter snapshots, and breach flags.
package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// SchemaVersion is the current version of MonitorState. Bump on
	// breaking field removals or type changes. Additive changes (new
	// fields) do not require a bump; readers tolerate unknown fields.
	SchemaVersion = 2

	// DefaultStateFilePath is the on-host location of the state file.
	// It matches the hostPath volume mount in the DaemonSet manifest.
	DefaultStateFilePath = "/var/run/nic_health_monitor/state.json"

	// DefaultBootIDPath is the sysfs node that exposes the kernel's
	// random boot ID. It is read once at startup and used to detect host
	// reboots (see Load). The DaemonSet bind-mounts /proc into
	// /nvsentinel/proc so tests can point at an alternate path.
	DefaultBootIDPath = "/nvsentinel/proc/sys/kernel/random/boot_id"
)

// MonitorState is the on-disk schema. Only fields defined in this
// struct survive a Load/Save cycle; unknown JSON fields are dropped.
type MonitorState struct {
	Version int    `json:"version"`
	BootID  string `json:"boot_id,omitempty"`

	// Scope fingerprints the discovery configuration (inclusion override
	// + exclusion regex) the state was recorded under. On Load, a scope
	// mismatch discards the port/device state (seeding it would fabricate
	// device-disappearance FATALs when the scope shrinks and bypass
	// first-poll severity gating when it grows) but PRESERVES counter
	// snapshots and latched breach flags: counters only reset on a real
	// reboot or an admin clear, and a scope change is neither.
	Scope string `json:"scope,omitempty"`

	// State detection state — produced by InfiniBandStateCheck and
	// EthernetStateCheck. Keys follow the `<device>_<port>` convention.
	PortStates   map[string]PortStateSnapshot `json:"port_states,omitempty"`
	KnownDevices []string                     `json:"known_devices,omitempty"`

	// AnomalousCards is the card-homogeneity latch: cards (PCI
	// bus:device) currently reported below their role group's decisive
	// mode via a FATAL card event. Keys are "<link_layer>/<card>" so the
	// IB and Ethernet checks can latch the same physical card (VPI/mixed
	// cards share a PCI bus:device across function suffixes) without
	// clobbering each other.
	//
	// Unlike port/device state, the latch survives boot-ID and scope
	// resets at Load: an entry means a card FATAL is outstanding
	// downstream (e.g., holding a quarantine). Within a boot, only the
	// matching card-healthy recovery (positive evidence: card present,
	// group decisive, at/above mode) removes an entry. On a baseline run
	// (first poll after a reboot or scope change) the check-scoped clear
	// voids every downstream condition and the latches are dropped with
	// it — still-anomalous cards immediately re-latch with fresh
	// entities.
	AnomalousCards map[string]AnomalousCardFlag `json:"anomalous_cards,omitempty"`

	// DisappearedDevices is the device-disappearance latch. It survives
	// restarts, reboots, and discovery-scope changes because each entry
	// represents a device-level FATAL still outstanding downstream. A
	// healthy port observed after re-enumeration removes the matching entry.
	DisappearedDevices map[string]DisappearedDeviceFlag `json:"disappeared_devices,omitempty"`

	// DisappearedPorts is the port-disappearance latch: ports that
	// vanished from a still-present device and have a port-scoped FATAL
	// outstanding downstream. A healthy observation of the port after it
	// reappears removes the entry. Keys are "<link_layer>/<device>_<port>".
	DisappearedPorts map[string]DisappearedPortFlag `json:"disappeared_ports,omitempty"`

	// DeviceMissCounts debounces confirmed enumeration misses. Unlike an
	// outstanding disappearance FATAL, a partial miss is operational polling
	// state and is discarded on boot-ID or discovery-scope changes.
	DeviceMissCounts map[string]DeviceMissCount `json:"device_miss_counts,omitempty"`

	// Counter detection state — produced by InfiniBandDegradationCheck
	// and EthernetDegradationCheck. Both maps key on
	// `<device>:<port>:<counter_name>` so the IB and Ethernet checks
	// keep distinct entries even when they share a state file.
	CounterSnapshots map[string]CounterSnapshot   `json:"counter_snapshots,omitempty"`
	BreachFlags      map[string]CounterBreachFlag `json:"breach_flags,omitempty"`

	// PendingBaselines records, per check name, that a baseline
	// reconciliation (check-scoped clear + replay) is owed but has not
	// run yet — the first polls after a reboot can be deferred or
	// partial while devices are unreadable, and the monitor may commit
	// partial-window state (which persists the NEW boot ID) before the
	// clear ever fires. Without this flag a pod restart inside that
	// window would compare equal boot IDs and silently drop the owed
	// clear, orphaning the previous boot's conditions downstream.
	PendingBaselines map[string]bool `json:"pending_baselines,omitempty"`
}

// PortStateSnapshot captures the last-known state of a port. LinkLayer
// lets each check filter the global map to its own ports (IB vs
// Ethernet) when seeding in-memory previous-state maps.
type PortStateSnapshot struct {
	Device        string `json:"device"`
	Port          int    `json:"port"`
	State         string `json:"state"`
	PhysicalState string `json:"physical_state"`
	LinkLayer     string `json:"link_layer,omitempty"`
}

// AnomalousCardFlag marks one latched card-homogeneity anomaly. The
// LinkLayer tag scopes entries to the state check that owns them (IB vs
// Ethernet), mirroring PortStateSnapshot; Card carries the PCI
// bus:device so lookups don't need to parse the composite map key.
type AnomalousCardFlag struct {
	Card      string `json:"card,omitempty"`
	LinkLayer string `json:"link_layer,omitempty"`
}

// DisappearedDeviceFlag marks a device-level disappearance FATAL that needs
// a future healthy re-enumeration event to clear it.
type DisappearedDeviceFlag struct {
	Device    string `json:"device,omitempty"`
	LinkLayer string `json:"link_layer,omitempty"`

	// ObservedThisBoot marks a latch created by the current boot's own
	// monitoring, as opposed to one carried over from a previous boot.
	// Baseline reconciliation re-asserts this-boot latches after the
	// check-scoped clear (their evidence is current) and drops
	// previous-boot ones with it. Load zeroes the marker when the boot
	// ID changes so provenance survives pod restarts but never a reboot.
	ObservedThisBoot bool `json:"observed_this_boot,omitempty"`
}

// DisappearedPortFlag marks a port-level disappearance FATAL (port gone
// from a still-present device) that needs a healthy observation of the
// reappeared port to clear it. ObservedThisBoot has the same semantics
// as on DisappearedDeviceFlag.
type DisappearedPortFlag struct {
	Device           string `json:"device,omitempty"`
	Port             int    `json:"port,omitempty"`
	LinkLayer        string `json:"link_layer,omitempty"`
	ObservedThisBoot bool   `json:"observed_this_boot,omitempty"`
}

// DeviceMissCount stores consecutive complete-enumeration polls in which a
// previously monitored device was absent.
type DeviceMissCount struct {
	Device    string `json:"device,omitempty"`
	LinkLayer string `json:"link_layer,omitempty"`
	Count     int    `json:"count,omitempty"`
}

// CounterSnapshot stores the value and wall-clock timestamp of a counter
// reading. For delta thresholds the snapshot is updated every poll;
// for velocity thresholds it is held for the configured velocityUnit
// window so the rate can be computed over real elapsed time.
type CounterSnapshot struct {
	Value     uint64    `json:"value"`
	Timestamp time.Time `json:"timestamp"`
}

// CounterBreachFlag tracks whether a counter is currently in a breached
// state. Breach is latching: once set, it stays set until the counter
// is reset (admin clear) or the host reboots. The CheckName and IsFatal
// fields preserve the original event's identity so the recovery event
// clears the same condition on the platform. Device and Port capture
// the original event's entities so baseline reconciliation can
// re-assert the breach even when the key is not readable on the
// reconciliation poll (absent device, missing counter file, vanished
// netdev) — including interface-level "net:*" keys whose entities are
// not reconstructible from the key alone.
type CounterBreachFlag struct {
	Breached  bool      `json:"breached"`
	CheckName string    `json:"check_name,omitempty"`
	IsFatal   bool      `json:"is_fatal,omitempty"`
	Since     time.Time `json:"since,omitempty"`
	Device    string    `json:"device,omitempty"`
	Port      string    `json:"port,omitempty"`
}

// Manager coordinates reads and writes to the shared state file. A
// single Manager instance is shared between all checks; its internal
// mutex keeps concurrent writes from corrupting the on-disk file.
type Manager struct {
	mu         sync.Mutex
	path       string
	bootIDPath string
	state      MonitorState
	loaded     bool

	// scope is the current discovery-scope fingerprint (see
	// MonitorState.Scope). Set via SetScope before Load.
	scope string

	// bootIDChanged captures the result of the most recent Load call so
	// callers that need to differentiate "fresh node or host reboot"
	// from "pod restart with persisted state" can query it.
	bootIDChanged bool

	// scopeChanged is true when the most recent Load discarded the
	// port/device state because the discovery-scope fingerprint changed.
	// Deliberately separate from bootIDChanged: state checks treat both
	// as "emit healthy baselines on the first poll", but counter checks
	// must react only to real reboots — a scope change neither resets
	// hardware counters nor clears latched breaches.
	scopeChanged bool
}

// NewManager constructs a Manager backed by the default on-host paths.
func NewManager() *Manager {
	return NewManagerWithPaths(DefaultStateFilePath, DefaultBootIDPath)
}

// NewManagerWithPaths constructs a Manager with explicit paths, used by
// tests to redirect to tempdir-backed files.
func NewManagerWithPaths(statePath, bootIDPath string) *Manager {
	return &Manager{
		path:       statePath,
		bootIDPath: bootIDPath,
		state:      MonitorState{Version: SchemaVersion},
	}
}

// SetScope records the discovery-scope fingerprint the monitor is
// running under. Must be called before Load; port/device state recorded
// under a different fingerprint is discarded at Load (counter state is
// preserved — see MonitorState.Scope). An empty scope matches legacy
// state files that predate the field.
func (m *Manager) SetScope(scope string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.scope = scope
}

// Path returns the state file path the Manager is configured to write.
func (m *Manager) Path() string {
	return m.path
}

// Paths returns the state file path and boot-ID file path.
func (m *Manager) Paths() (string, string) {
	return m.path, m.bootIDPath
}

// Load reads the persisted state file, compares its boot ID against the
// current kernel boot ID, and seeds the Manager's in-memory state. The
// returned error is non-nil only on I/O or JSON-parse failures that the
// caller should surface; "file missing", "file corrupt", and "boot ID
// changed" are all treated as recoverable conditions that reset the
// state to empty and log a warning.
//
// After Load, BootIDChanged reports whether the persisted state was
// discarded for any of the reasons above. Callers that drive the
// "first poll after boot" healthy-baseline behaviour should consult it
// exactly once at startup.
func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentBootID, err := m.readBootID()
	if err != nil {
		// Without a boot ID we can't reason about reboots. Start empty
		// and treat every restart as a fresh one (safe direction).
		slog.Warn("Could not read boot ID, treating startup as fresh boot",
			"path", m.bootIDPath, "error", err)

		m.resetStateLocked("")

		return nil
	}

	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Info("State file does not exist, starting with empty state",
				"path", m.path)
		} else {
			slog.Warn("Could not read state file, starting with empty state",
				"path", m.path, "error", err)
		}

		m.resetStateLocked(currentBootID)

		return nil
	}

	var loaded MonitorState
	if err := json.Unmarshal(data, &loaded); err != nil {
		slog.Warn("State file is corrupt, discarding contents",
			"path", m.path, "error", err)

		m.resetStateLocked(currentBootID)

		return nil
	}

	if loaded.BootID != currentBootID {
		slog.Info("Boot ID changed, resetting persisted state "+
			"(outstanding state-event latches preserved)",
			"previous_boot_id", loaded.BootID,
			"current_boot_id", currentBootID,
		)

		// Card-anomaly and device/port-disappearance latches survive the
		// reboot load: they track FATALs outstanding downstream (e.g., a
		// quarantine annotation), which a reboot does not clear. The first
		// baseline poll emits the check-scoped clear that voids those
		// conditions and consumes the latches. Their this-boot provenance
		// markers are zeroed — whatever they observed belongs to the
		// previous boot now. All observational state resets as usual.
		m.state = MonitorState{
			Version:            SchemaVersion,
			BootID:             currentBootID,
			Scope:              m.scope,
			AnomalousCards:     loaded.AnomalousCards,
			DisappearedDevices: stripDeviceBootMarkers(loaded.DisappearedDevices),
			DisappearedPorts:   stripPortBootMarkers(loaded.DisappearedPorts),
		}
		m.loaded = true
		m.bootIDChanged = true
		m.scopeChanged = false

		return nil
	}

	if loaded.Version != SchemaVersion {
		slog.Info("Schema version changed, discarding stale state",
			"file_version", loaded.Version,
			"current_version", SchemaVersion,
		)

		m.resetStateLocked(currentBootID)

		return nil
	}

	if loaded.Scope != m.scope {
		slog.Info("Discovery scope changed, resetting port/device state "+
			"(counter snapshots and breach latches preserved)",
			"previous_scope", loaded.Scope,
			"current_scope", m.scope,
		)

		// PortStates and KnownDevices are intentionally dropped with the
		// scope: the state checks re-baseline. Counter state survives
		// (see MonitorState.Scope), and so do outstanding state-event latches —
		// a scope change (e.g., enabling the inclusion override, whose
		// mode skips the card lifecycle entirely) must not orphan a card
		// FATAL that is still holding a quarantine downstream. The
		// disappearance latch is preserved for the same reason, and a
		// pending baseline reconciliation owed from a reboot must not be
		// forgotten because the operator changed the scope meanwhile.
		m.state = MonitorState{
			Version:            SchemaVersion,
			BootID:             currentBootID,
			Scope:              m.scope,
			AnomalousCards:     loaded.AnomalousCards,
			DisappearedDevices: loaded.DisappearedDevices,
			DisappearedPorts:   loaded.DisappearedPorts,
			CounterSnapshots:   loaded.CounterSnapshots,
			BreachFlags:        loaded.BreachFlags,
			PendingBaselines:   loaded.PendingBaselines,
		}
		m.loaded = true
		m.bootIDChanged = false
		m.scopeChanged = true

		return nil
	}

	m.state = loaded
	m.state.Scope = m.scope
	m.loaded = true
	m.bootIDChanged = false
	m.scopeChanged = false

	slog.Info("Loaded persisted state",
		"path", m.path,
		"known_devices", len(loaded.KnownDevices),
		"port_states", len(loaded.PortStates),
	)

	return nil
}

// BootIDChanged reports whether the most recent Load discarded the
// entire persisted state and treated this startup as a fresh boot —
// missing/corrupt file, boot-ID change, or schema version change.
// A discovery-scope change is NOT included: it is reported separately
// via ScopeChanged because counters must not treat it as a reboot.
// Must be called after Load.
func (m *Manager) BootIDChanged() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.bootIDChanged
}

// ScopeChanged reports whether the most recent Load discarded the
// port/device state because the discovery-scope fingerprint changed
// (counter state was preserved). Must be called after Load.
func (m *Manager) ScopeChanged() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.scopeChanged
}

// PortStatesFor returns a copy of persisted port snapshots whose
// LinkLayer matches one of the given layers (case-insensitive). An
// empty layers slice returns every entry. The returned map is safe for
// the caller to mutate.
func (m *Manager) PortStatesFor(layers ...string) map[string]PortStateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]PortStateSnapshot, len(m.state.PortStates))

	for k, v := range m.state.PortStates {
		if !matchesLayer(v.LinkLayer, layers) {
			continue
		}

		out[k] = v
	}

	return out
}

// KnownDevices returns a copy of the persisted KnownDevices list. The
// state checks merge this with the devices they discover live on each
// poll to detect disappearance across pod restarts.
func (m *Manager) KnownDevices() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.state.KnownDevices...)
}

// UpdatePortStates merges per-check port state into the shared map,
// replacing any existing entries that match the provided LinkLayer(s).
// Entries with a different LinkLayer (written by the sibling check) are
// preserved. knownDevices is unioned with the persisted list so the
// state checks do not clobber each other's device sets.
// UpdatePortStates merges per-check port state into the shared map,
// replacing any existing entries that match the provided LinkLayer(s).
// Returns true if the state was modified (caller should Save).
func (m *Manager) UpdatePortStates(
	portStates map[string]PortStateSnapshot,
	knownDevices []string,
	layers ...string,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.PortStates == nil {
		m.state.PortStates = make(map[string]PortStateSnapshot, len(portStates))
	}

	if !m.portStatesChanged(portStates, layers) {
		return false
	}

	for k, v := range m.state.PortStates {
		if matchesLayer(v.LinkLayer, layers) {
			delete(m.state.PortStates, k)
		}
	}

	for k, v := range portStates {
		m.state.PortStates[k] = v
	}

	// Rebuild KnownDevices from the current PortStates rather than
	// merging with stale entries. This ensures disappeared devices are
	// removed from the persisted list.
	seen := make(map[string]bool)
	for _, v := range m.state.PortStates {
		seen[v.Device] = true
	}

	devices := make([]string, 0, len(seen))
	for d := range seen {
		devices = append(devices, d)
	}

	sort.Strings(devices)
	m.state.KnownDevices = devices

	return true
}

// portStatesChanged reports whether the incoming port states differ from
// the currently persisted entries for the given link layers.
func (m *Manager) portStatesChanged(
	incoming map[string]PortStateSnapshot, layers []string,
) bool {
	for k, old := range m.state.PortStates {
		if !matchesLayer(old.LinkLayer, layers) {
			continue
		}

		if newSnap, exists := incoming[k]; !exists || old != newSnap {
			return true
		}
	}

	for k := range incoming {
		if _, exists := m.state.PortStates[k]; !exists {
			return true
		}
	}

	return false
}

// anomalousCardKey builds the persisted latch key. Including the link
// layer keeps the IB and Ethernet checks' entries for the same physical
// card (VPI/mixed cards share a PCI bus:device) from overwriting each
// other.
func anomalousCardKey(linkLayer, card string) string {
	return linkLayer + "/" + card
}

// AnomalousCardsFor returns the persisted card-anomaly latch entries
// whose LinkLayer matches one of the given layers (case-insensitive;
// empty filter matches everything), keyed by card.
func (m *Manager) AnomalousCardsFor(layers ...string) map[string]AnomalousCardFlag {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]AnomalousCardFlag, len(m.state.AnomalousCards))

	for _, v := range m.state.AnomalousCards {
		if !matchesLayer(v.LinkLayer, layers) {
			continue
		}

		out[v.Card] = v
	}

	return out
}

// UpdateAnomalousCards replaces the persisted card-anomaly latch entries
// matching the provided LinkLayer(s) with the incoming card-keyed set,
// preserving entries owned by the sibling check (including entries for
// the same card under the other link layer). Returns true if the state
// was modified (caller should Save).
func (m *Manager) UpdateAnomalousCards(
	cards map[string]AnomalousCardFlag,
	layers ...string,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.AnomalousCards == nil {
		m.state.AnomalousCards = make(map[string]AnomalousCardFlag, len(cards))
	}

	changed := false

	for k, v := range m.state.AnomalousCards {
		if !matchesLayer(v.LinkLayer, layers) {
			continue
		}

		if _, keep := cards[v.Card]; !keep {
			delete(m.state.AnomalousCards, k)

			changed = true
		}
	}

	for card, v := range cards {
		v.Card = card

		key := anomalousCardKey(v.LinkLayer, card)
		if old, exists := m.state.AnomalousCards[key]; !exists || old != v {
			m.state.AnomalousCards[key] = v
			changed = true
		}
	}

	return changed
}

func linkLayerEntityKey(linkLayer, entity string) string {
	return linkLayer + "/" + entity
}

// DisappearedDevicesFor returns outstanding device-disappearance latches for
// the requested link layer(s), keyed by device name.
func (m *Manager) DisappearedDevicesFor(layers ...string) map[string]DisappearedDeviceFlag {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]DisappearedDeviceFlag, len(m.state.DisappearedDevices))
	for _, v := range m.state.DisappearedDevices {
		if matchesLayer(v.LinkLayer, layers) {
			out[v.Device] = v
		}
	}

	return out
}

// UpdateDisappearedDevices replaces the caller's link-layer slice of the
// persisted device-disappearance latch while preserving sibling-check entries.
func (m *Manager) UpdateDisappearedDevices(
	devices map[string]DisappearedDeviceFlag,
	layers ...string,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.DisappearedDevices == nil {
		m.state.DisappearedDevices = make(map[string]DisappearedDeviceFlag, len(devices))
	}

	changed := false

	for k, v := range m.state.DisappearedDevices {
		if matchesLayer(v.LinkLayer, layers) {
			if _, keep := devices[v.Device]; !keep {
				delete(m.state.DisappearedDevices, k)

				changed = true
			}
		}
	}

	for device, v := range devices {
		v.Device = device

		key := linkLayerEntityKey(v.LinkLayer, device)
		if old, exists := m.state.DisappearedDevices[key]; !exists || old != v {
			m.state.DisappearedDevices[key] = v
			changed = true
		}
	}

	return changed
}

// stripDeviceBootMarkers zeroes the this-boot provenance marker on
// device-disappearance latches carried across a boot-ID change.
func stripDeviceBootMarkers(flags map[string]DisappearedDeviceFlag) map[string]DisappearedDeviceFlag {
	out := make(map[string]DisappearedDeviceFlag, len(flags))

	for k, v := range flags {
		v.ObservedThisBoot = false
		out[k] = v
	}

	return out
}

// stripPortBootMarkers is stripDeviceBootMarkers for port latches.
func stripPortBootMarkers(flags map[string]DisappearedPortFlag) map[string]DisappearedPortFlag {
	out := make(map[string]DisappearedPortFlag, len(flags))

	for k, v := range flags {
		v.ObservedThisBoot = false
		out[k] = v
	}

	return out
}

// disappearedPortKey builds the persisted key for a port latch. The
// device_port portion matches the state checks' port key convention.
func disappearedPortKey(linkLayer, device string, port int) string {
	return fmt.Sprintf("%s/%s_%d", linkLayer, device, port)
}

// DisappearedPortsFor returns outstanding port-disappearance latches for
// the requested link layer(s), keyed by "<device>_<port>".
func (m *Manager) DisappearedPortsFor(layers ...string) map[string]DisappearedPortFlag {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]DisappearedPortFlag, len(m.state.DisappearedPorts))

	for _, v := range m.state.DisappearedPorts {
		if matchesLayer(v.LinkLayer, layers) {
			out[fmt.Sprintf("%s_%d", v.Device, v.Port)] = v
		}
	}

	return out
}

// UpdateDisappearedPorts replaces the caller's link-layer slice of the
// persisted port-disappearance latch while preserving sibling-check
// entries. The input map is keyed by "<device>_<port>".
func (m *Manager) UpdateDisappearedPorts(
	ports map[string]DisappearedPortFlag,
	layers ...string,
) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.DisappearedPorts == nil {
		m.state.DisappearedPorts = make(map[string]DisappearedPortFlag, len(ports))
	}

	changed := false

	incoming := make(map[string]DisappearedPortFlag, len(ports))
	for _, v := range ports {
		incoming[disappearedPortKey(v.LinkLayer, v.Device, v.Port)] = v
	}

	for k, v := range m.state.DisappearedPorts {
		if matchesLayer(v.LinkLayer, layers) {
			if _, keep := incoming[k]; !keep {
				delete(m.state.DisappearedPorts, k)

				changed = true
			}
		}
	}

	for k, v := range incoming {
		if old, exists := m.state.DisappearedPorts[k]; !exists || old != v {
			m.state.DisappearedPorts[k] = v
			changed = true
		}
	}

	return changed
}

// DeviceMissCountsFor returns consecutive enumeration-miss counts for the
// requested link layer(s), keyed by device name.
func (m *Manager) DeviceMissCountsFor(layers ...string) map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]int, len(m.state.DeviceMissCounts))
	for _, v := range m.state.DeviceMissCounts {
		if matchesLayer(v.LinkLayer, layers) {
			out[v.Device] = v.Count
		}
	}

	return out
}

// UpdateDeviceMissCounts replaces the caller's link-layer slice of persisted
// debounce state while preserving sibling-check entries.
func (m *Manager) UpdateDeviceMissCounts(counts map[string]int, linkLayer string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.DeviceMissCounts == nil {
		m.state.DeviceMissCounts = make(map[string]DeviceMissCount, len(counts))
	}

	changed := false

	for k, v := range m.state.DeviceMissCounts {
		if strings.EqualFold(v.LinkLayer, linkLayer) {
			if _, keep := counts[v.Device]; !keep {
				delete(m.state.DeviceMissCounts, k)

				changed = true
			}
		}
	}

	for device, count := range counts {
		v := DeviceMissCount{Device: device, LinkLayer: linkLayer, Count: count}

		key := linkLayerEntityKey(linkLayer, device)
		if old, exists := m.state.DeviceMissCounts[key]; !exists || old != v {
			m.state.DeviceMissCounts[key] = v
			changed = true
		}
	}

	return changed
}

// PendingBaseline reports whether the named check still owes a baseline
// reconciliation (check-scoped clear + replay). Checks consult this at
// construction so a pod restart inside a deferred/partial baseline
// window cannot lose the owed clear.
func (m *Manager) PendingBaseline(check string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.state.PendingBaselines[check]
}

// SetPendingBaseline records that the named check owes a baseline
// reconciliation. Persisted on the next Save.
func (m *Manager) SetPendingBaseline(check string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.PendingBaselines == nil {
		m.state.PendingBaselines = make(map[string]bool)
	}

	m.state.PendingBaselines[check] = true
}

// ClearPendingBaseline records that the named check completed its
// baseline reconciliation. Persisted on the next Save.
func (m *Manager) ClearPendingBaseline(check string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.state.PendingBaselines, check)
}

// CounterSnapshots returns a copy of the persisted counter snapshots.
// Each evaluator seeds its in-memory snapshot map from this on startup
// so that delta and velocity windows survive pod restarts.
func (m *Manager) CounterSnapshots() map[string]CounterSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]CounterSnapshot, len(m.state.CounterSnapshots))
	for k, v := range m.state.CounterSnapshots {
		out[k] = v
	}

	return out
}

// UpdateCounterSnapshots merges the supplied counter snapshots into the
// shared map. The merge iterates only over the incoming map, so callers
// (the IB and Ethernet evaluators) must scope their input to the keys
// they actually own — otherwise one evaluator's loaded-but-not-written
// view of a sibling key could clobber the sibling's update. Returns
// true if any persisted snapshot value or timestamp actually changed;
// callers only Save when true.
func (m *Manager) UpdateCounterSnapshots(snapshots map[string]CounterSnapshot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.CounterSnapshots == nil {
		m.state.CounterSnapshots = make(map[string]CounterSnapshot, len(snapshots))
	}

	changed := false

	for k, v := range snapshots {
		old, exists := m.state.CounterSnapshots[k]
		if !exists || old.Value != v.Value || !old.Timestamp.Equal(v.Timestamp) {
			m.state.CounterSnapshots[k] = v
			changed = true
		}
	}

	return changed
}

// BreachFlags returns a copy of the persisted breach flags so an
// evaluator can rehydrate its in-memory view on startup.
func (m *Manager) BreachFlags() map[string]CounterBreachFlag {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]CounterBreachFlag, len(m.state.BreachFlags))
	for k, v := range m.state.BreachFlags {
		out[k] = v
	}

	return out
}

// UpdateBreachFlags merges incoming breach flags. Incoming entries with
// Breached=false delete any matching persisted entry (signalling a
// recovered counter), so callers MUST submit such entries explicitly
// rather than just dropping cleared flags from their input map — the
// merge cannot infer a deletion from an absent key. Returns true if
// any entry was added, removed, or had its fields changed.
func (m *Manager) UpdateBreachFlags(flags map[string]CounterBreachFlag) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state.BreachFlags == nil {
		m.state.BreachFlags = make(map[string]CounterBreachFlag, len(flags))
	}

	changed := false

	for k, v := range flags {
		old, exists := m.state.BreachFlags[k]
		switch {
		case !v.Breached && exists:
			delete(m.state.BreachFlags, k)

			changed = true
		case v.Breached && (!exists || old != v):
			m.state.BreachFlags[k] = v
			changed = true
		}
	}

	return changed
}

// Save writes the current state to disk atomically (tmp file + rename).
// Errors are returned for the caller to log; the design explicitly
// chooses not to halt monitoring on persistence failures.
func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.saveLocked()
}

// resetStateLocked initialises an empty state with the given boot ID
// and marks the manager as having just transitioned to a fresh boot.
// Callers must hold m.mu.
func (m *Manager) resetStateLocked(bootID string) {
	m.state = MonitorState{
		Version: SchemaVersion,
		BootID:  bootID,
		Scope:   m.scope,
	}
	m.loaded = true
	m.bootIDChanged = true
	m.scopeChanged = false
}

// saveLocked serialises m.state to disk using the atomic-rename pattern:
// write to a sibling .tmp file, fsync it, rename it onto the real path.
// Callers must hold m.mu.
func (m *Manager) saveLocked() error {
	if !m.loaded {
		return fmt.Errorf("state file not loaded; call Load before Save")
	}

	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal monitor state: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return fmt.Errorf("create state dir %s: %w", filepath.Dir(m.path), err)
	}

	tmp := m.path + ".tmp"
	if err := writeFileAtomic(tmp, data); err != nil {
		return err
	}

	if err := os.Rename(tmp, m.path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, m.path, err)
	}

	return nil
}

// writeFileAtomic writes data to path and fsyncs the file before close,
// so a crash between WriteFile and Rename cannot leave a zero-length
// state file on the next boot.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return fmt.Errorf("write %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()

		return fmt.Errorf("fsync %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}

	return nil
}

// readBootID reads and trims the contents of the boot ID sysfs file.
func (m *Manager) readBootID() (string, error) {
	data, err := os.ReadFile(m.bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", m.bootIDPath, err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("%s contained empty boot ID", m.bootIDPath)
	}

	return id, nil
}

// matchesLayer reports whether the given port's LinkLayer matches any
// of the filter strings (case-insensitive). An empty filter matches
// everything.
func matchesLayer(portLayer string, layers []string) bool {
	if len(layers) == 0 {
		return true
	}

	lower := strings.ToLower(strings.TrimSpace(portLayer))

	for _, l := range layers {
		if strings.EqualFold(strings.TrimSpace(l), lower) {
			return true
		}
	}

	return false
}
