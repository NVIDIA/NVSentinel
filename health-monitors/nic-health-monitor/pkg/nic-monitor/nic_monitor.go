/*
 * Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nic_monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/klog"
)

type NicType int

const (
	// states
	doesNotExistState = "state: Does Not Exist"
	existsState       = "state: Exists"
)

// MonitorNetworkType defines the types of network to monitor
type MonitorNetworkType string

const (
	// MonitorNetworkTypeRoCE monitors Ethernet NICs and InfiniBand NICs with link_layer Ethernet (RoCE)
	MonitorNetworkTypeRoCE MonitorNetworkType = "roce"
	// MonitorNetworkTypeInfiniBand monitors InfiniBand NICs with link_layer InfiniBand
	MonitorNetworkTypeInfiniBand MonitorNetworkType = "infiniband"
	// MonitorNetworkTypeAll monitors all Ethernet and InfiniBand NICs (default)
	MonitorNetworkTypeAll MonitorNetworkType = "all"
)

const (
	Ethernet NicType = iota
	Infiniband
)

var pollingLoopProcessingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "nic_monitor_polling_loop_processing_duration_milliseconds",
	Help:    "The processing time for each polling loop in milliseconds (excluding the polling interval wait time)",
	Buckets: prometheus.LinearBuckets(0, 10, 500),
})

type NicMonitorConfig struct {
	ExclusionRegexes                                 []string
	PollingIntervalInMilliseconds                    int
	MaxRetryDurationForDownDetectedNICInMilliseconds int
	RetryIntervalForDownDetectedNICInMilliseconds    int
	MaxRetriesForRetryableError                      int
	RetryDelaySecondsForRetryableError               int
	SysClassNetPath                                  string
	SysClassInfinibandPath                           string
	StateFilePath                                    string
	MonitorNetworkType                               MonitorNetworkType
}

var storedBootID string

// Definitive single block for these OS function vars
var (
	osReadFile  = os.ReadFile
	osWriteFile = os.WriteFile
	osMkdirAll  = os.MkdirAll
	osStat      = os.Stat
)

type NicMonitorState struct {
	Version int    `json:"version"`
	BootID  string `json:"boot_id"`
}

const (
	nicMonitorStateFileVersion = 1
)

type NicMonitor interface {
	Monitor(config *NicMonitorConfig) ([]NicHealthEvent, error)
}

type NicHealthMonitor struct {
	EventChan     chan *[]NicHealthEvent
	Monitors      []NicMonitor
	monitorConfig *NicMonitorConfig
	// Fields to handle initial reboot detection
	initialRebootDetected  bool
	previousBootIDForEvent string
}

func NewNicHealthMonitor(config *NicMonitorConfig) (*NicHealthMonitor, error) {
	collector := &NicHealthMonitor{
		EventChan:     make(chan *[]NicHealthEvent),
		monitorConfig: config,
	}

	// Load initial state for boot ID check
	initialState, err := loadNicMonitorState(config.StateFilePath)
	if err != nil {
		klog.Errorf(
			"Failed to load initial NIC monitor state from %s: %v. Will attempt to create/use default.",
			config.StateFilePath,
			err,
		)

		initialState = NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""}
	}

	currentLiveBootID, bootIDErr := fetchCurrentBootID()
	if bootIDErr != nil {
		klog.Errorf(
			"Failed to fetch current boot ID on startup: %v. State file will not be updated at this time.",
			bootIDErr,
		)

		storedBootID = initialState.BootID
	} else {
		if initialState.BootID != currentLiveBootID {
			klog.Infof("Reboot detected at startup. Previous BootID: %s, "+
				"Current BootID: %s", initialState.BootID, currentLiveBootID)

			collector.initialRebootDetected = true
			collector.previousBootIDForEvent = initialState.BootID
		}

		// Always update the state file with the current live boot ID if fetched successfully.
		stateToSave := NicMonitorState{
			Version: nicMonitorStateFileVersion,
			BootID:  currentLiveBootID,
		}

		if err := saveNicMonitorState(config.StateFilePath, stateToSave); err != nil {
			klog.Errorf("Failed to save NIC monitor state to %s on startup: %v", config.StateFilePath, err)
		}

		storedBootID = currentLiveBootID
	}

	scanAndRegisterNics(collector)

	return collector, nil
}

func scanAndRegisterNics(collector *NicHealthMonitor) {
	// Determine what to monitor based on configuration and then register monitors.
	monitorEth, monitorIB := networkTypesToMonitor(collector)

	if monitorIB {
		collector.registerInfinibandMonitor()
	}

	if monitorEth {
		collector.registerEthernetMonitor()
	}
}

// networkTypesToMonitor decides which network types (Ethernet / InfiniBand) should be
// monitored based on the supplied configuration. It always errs on the side of
// monitoring "everything" when the configuration is missing or invalid so that we
// never accidentally miss devices.
func networkTypesToMonitor(c *NicHealthMonitor) (monitorEth, monitorIB bool) {
	if c == nil || c.monitorConfig == nil {
		klog.Error("Monitor config is nil, defaulting to monitoring all network types.")
		return true, true
	}

	switch c.monitorConfig.MonitorNetworkType {
	case MonitorNetworkTypeRoCE:
		klog.Info("Monitoring RoCE – Ethernet NICs and InfiniBand NICs with Ethernet link_layer.")
		return true, true // IB monitor filters by link_layer internally.
	case MonitorNetworkTypeInfiniBand:
		klog.Info("Monitoring InfiniBand NICs with InfiniBand link_layer only.")
		return false, true
	case MonitorNetworkTypeAll:
		klog.Info("Monitoring all Ethernet and InfiniBand NICs.")
		return true, true
	default:
		klog.Warningf("Unknown MonitorNetworkType '%s', defaulting to all.",
			c.monitorConfig.MonitorNetworkType)
		return true, true
	}
}

// registerInfinibandMonitor appends an InfinibandDeviceMonitor if the kernel
// exposes the relevant sysfs path.
func (c *NicHealthMonitor) registerInfinibandMonitor() {
	if !dirExists(c.monitorConfig.SysClassInfinibandPath) {
		return
	}

	klog.Info("Registering InfiniBand device monitor.")

	c.Monitors = append(c.Monitors, &InfinibandDeviceMonitor{})
}

// registerEthernetMonitor appends an EthernetDeviceMonitor when /sys/class/net
// exists.
func (c *NicHealthMonitor) registerEthernetMonitor() {
	if !dirExists(c.monitorConfig.SysClassNetPath) {
		return
	}

	klog.Info("Registering Ethernet device monitor.")

	c.Monitors = append(c.Monitors, &EthernetDeviceMonitor{})
}

// dirExists is a thin wrapper around os.Stat that logs helpful messages and
// distinguishes between a non-existent path (normal) and an actual error.
func dirExists(path string) bool {
	_, err := osStat(path)
	if err == nil {
		return true
	}

	if os.IsNotExist(err) {
		klog.V(1).Infof("%s does not exist – skipping monitor registration.", path)
		return false
	}

	klog.Errorf("Error checking path %s: %v", path, err)

	return false
}

// emitInitialRebootEvent sends a single reboot health-event if NewNicHealthMonitor
// detected a reboot during construction.
func (c *NicHealthMonitor) emitInitialRebootEvent() {
	if !c.initialRebootDetected {
		return
	}

	c.initialRebootDetected = false // ensure we only emit once
	c.emitRebootEvents(c.previousBootIDForEvent, storedBootID)
}

// detectAndHandleReboot checks if the machine rebooted (bootID changed) and, if
// so, updates persistent state and emits health events.
func (c *NicHealthMonitor) detectAndHandleReboot() {
	currentBootID, err := fetchCurrentBootID()
	if err != nil {
		klog.Errorf("Failed to fetch current boot ID: %v", err)
		return
	}

	if storedBootID == currentBootID {
		return
	}

	prevBootID := storedBootID
	storedBootID = currentBootID

	state := NicMonitorState{Version: nicMonitorStateFileVersion, BootID: currentBootID}
	if errSave := saveNicMonitorState(c.monitorConfig.StateFilePath, state); errSave != nil {
		klog.Errorf("Failed to save NIC monitor state after boot ID change: %v", errSave)
	}

	c.emitRebootEvents(prevBootID, currentBootID)
}

// emitRebootEvents helper builds and sends reboot events for all registered
// monitors.
func (c *NicHealthMonitor) emitRebootEvents(prevBootID, currentBootID string) {
	var rebootEvents []NicHealthEvent

	for _, monitor := range c.Monitors {
		event := NicHealthEvent{
			Message: fmt.Sprintf(
				"System reboot detected. BootID changed from %s to %s",
				prevBootID,
				currentBootID,
			),
			IsHealthyEvent: true,
		}

		switch monitor.(type) {
		case *EthernetDeviceMonitor:
			event.NicType = Ethernet
		case *InfinibandDeviceMonitor:
			event.NicType = Infiniband
		}

		rebootEvents = append(rebootEvents, event)
	}

	if len(rebootEvents) > 0 {
		c.EventChan <- &rebootEvents
	}
}

// collectMonitorEvents iterates over all device monitors and forwards any events
// they return onto the central EventChan.
func (c *NicHealthMonitor) collectMonitorEvents() error {
	for _, monitor := range c.Monitors {
		events, err := monitor.Monitor(c.monitorConfig)
		if err != nil {
			return fmt.Errorf("error occurred while monitoring: %w", err)
		}

		if len(events) > 0 {
			c.EventChan <- &events
		}
	}

	return nil
}

func (c *NicHealthMonitor) Close() error {
	return nil
}

func (c *NicHealthMonitor) Run() error {
	klog.Info("Collecting NIC events")

	c.emitInitialRebootEvent()

	ticker := time.NewTicker(time.Duration(c.monitorConfig.PollingIntervalInMilliseconds) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		c.detectAndHandleReboot()

		start := time.Now()

		if err := c.collectMonitorEvents(); err != nil {
			return err
		}

		duration := float64(time.Since(start).Milliseconds())
		pollingLoopProcessingDuration.Observe(duration)
	}

	return nil
}

// check if a nic name matches any exclusion regex
func isExcluded(name string, exclusionRegexes []string) bool {
	for _, regex := range exclusionRegexes {
		if match, _ := regexp.MatchString(regex, name); match {
			return true
		}
	}

	return false
}

type NicHealthEvent struct {
	NicType        NicType // e.g., "Ethernet", "Infiniband"
	Name           string
	Message        string
	IsHealthyEvent bool
}

var fetchCurrentBootID = func() (string, error) {
	data, err := osReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("failed to read boot_id: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

func saveNicMonitorState(stateFilePath string, state NicMonitorState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	if err := osMkdirAll(filepath.Dir(stateFilePath), 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	if err := osWriteFile(stateFilePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write state to file: %w", err)
	}

	return nil
}

func loadNicMonitorState(stateFilePath string) (NicMonitorState, error) {
	var state NicMonitorState

	data, err := osReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// File doesn't exist, return empty state, it will be created on first save
			klog.Infof("State file %s does not exist, creating with default state.", stateFilePath)
			return NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""}, nil
		}

		return state, fmt.Errorf("failed to read state from file: %w", err)
	}

	// Check if file is empty
	if len(data) == 0 {
		klog.Warningf("State file %s exists but is empty, treating as non-existent", stateFilePath)

		return NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""}, nil
	}

	if err := json.Unmarshal(data, &state); err != nil {
		klog.Warningf("State file %s is corrupted: %v, resetting to default state", stateFilePath, err)
		return NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""}, nil
	}

	if state.Version != nicMonitorStateFileVersion {
		klog.Warningf(
			"State file version mismatch: expected %d, got %d. Resetting state.",
			nicMonitorStateFileVersion,
			state.Version,
		)

		return NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""}, nil
	}

	return state, nil
}
