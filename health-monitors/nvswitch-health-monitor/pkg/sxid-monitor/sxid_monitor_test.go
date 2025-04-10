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

package sxid_monitor

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLogCollector(t *testing.T) {
	testParsingSXIDLogline2Metrics(t)
}

func testParsingSXIDLogline2Metrics(t *testing.T) {
	// Logs that needed to be parsed correctly
	logOK1 := "<12>[38889.018130] nvidia-nvswitch0: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate"
	logOK2 := "<12>[38889.018130] nvidia-nvswitch1: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 Another example error message"

	metric0 := SXIDErrorEvent{
		ErrorNum: 20009,
		IsFatal:  false,
		NVSwitch: 0,
		PCI:      "0000:06:00.0",
		Link:     4,
		Message:  "RX Short Error Rate",
	}

	metric1 := SXIDErrorEvent{
		ErrorNum: 20009,
		IsFatal:  false,
		NVSwitch: 1,
		PCI:      "0000:06:00.0",
		Link:     4,
		Message:  "Another example error message",
	}

	m, err := ParseSXIDError(logOK1)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, metric0, *m)

	m, err = ParseSXIDError(logOK2)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, metric1, *m)

	// Logs that do not return metric
	logNoSXidContinue := "<12>[38889.018130] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 12033, Severity 1 Engine instance 00 Sub-engine instance 00"
	logTruncated := "<12>[38889.018130] nvidia-nvswitch1: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Li"

	m, err = ParseSXIDError(logNoSXidContinue)
	require.NoError(t, err)
	require.Nil(t, m)

	_, err = ParseSXIDError(logTruncated)
	require.Error(t, err)

	// Logs that need to return error
	logMissingLink := "<12>[38889.018130] nvidia-nvswitch1: SXid (PCI:0000:07:00.0): 10001, Non-fatal, PRI WRITE SYSB error, instance=3, chiplet=1"

	_, err = ParseSXIDError(logMissingLink)
	require.Error(t, err)
	require.Equal(t, errors.New("link information is missing"), err)
}

func TestStatePersistence(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	initialState := nvSwitchMonitorState{
		LastTimestamp: 12345.6789,
		LastLogLine:   "test log line",
	}

	err := saveState(testStateFilePath, initialState)
	require.NoError(t, err)

	loadedState, err := loadState(testStateFilePath)
	require.NoError(t, err)
	require.Equal(t, initialState, loadedState)
}

func TestExtractTimestamp(t *testing.T) {
	log := "<12>[73309.599396] nvidia-nvswitch0: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate"

	timestamp, err := extractTimestamp(log)
	require.NoError(t, err)
	require.Equal(t, 73309.599396, timestamp)

	invalidLog := "Invalid log line"
	_, err = extractTimestamp(invalidLog)
	require.Error(t, err)
}

func TestSxidErrorMonitorInitialization(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	config := &SxidEventMonitorConfig{
		StateFilePath:                 testStateFilePath,
		PollingIntervalInMilliseconds: 1000,
	}
	monitor, err := NewSxidEventMonitor(config)
	require.NoError(t, err)
	require.NotNil(t, monitor)
	defer monitor.Close()

	require.Equal(t, float64(0), monitor.lastTimestamp)
	require.Equal(t, "", monitor.lastLogLine)
	require.Equal(t, testStateFilePath, monitor.stateFilePath)
	require.Equal(t, 1000, monitor.pollingIntervalInMilliseconds)
}

func TestProcessLog(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	config := &SxidEventMonitorConfig{
		StateFilePath:                 testStateFilePath,
		PollingIntervalInMilliseconds: 1000,
	}
	monitor, err := NewSxidEventMonitor(config)
	require.NoError(t, err)
	require.NotNil(t, monitor)
	defer monitor.Close()

	log := "<12>[73309.599396] nvidia-nvswitch0: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate"

	expectedEvent := &SXIDErrorEvent{
		ErrorNum: 20009,
		IsFatal:  false,
		NVSwitch: 0,
		PCI:      "0000:06:00.0",
		Link:     4,
		Message:  "RX Short Error Rate",
	}

	// start a goroutine to read from the event channel to prevent blocking
	done := make(chan struct{})
	var receivedEvent *SXIDErrorEvent
	go func() {
		receivedEvent = <-monitor.EventChan
		close(done)
	}()

	err = monitor.processLog(log)
	require.NoError(t, err)

	require.Equal(t, 73309.599396, monitor.lastTimestamp)
	require.Equal(t, log, monitor.lastLogLine)

	state, err := loadState(testStateFilePath)
	require.NoError(t, err)
	require.Equal(t, 73309.599396, state.LastTimestamp)
	require.Equal(t, log, state.LastLogLine)

	select {
	case <-done:
		require.NotNil(t, receivedEvent)
		require.Equal(t, expectedEvent, receivedEvent)
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for event")
	}
}

func TestBootIDChangeEmitsEvent(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	initialState := nvSwitchMonitorState{
		LastTimestamp: 12345.6789,
		LastLogLine:   "initial log line",
		BootID:        "old-boot-id",
	}

	err := saveState(testStateFilePath, initialState)
	require.NoError(t, err)

	config := &SxidEventMonitorConfig{
		StateFilePath:                 testStateFilePath,
		PollingIntervalInMilliseconds: 1000,
	}
	monitor, err := NewSxidEventMonitor(config)
	require.NoError(t, err)
	defer monitor.Close()

	// set up a channel to capture the event
	done := make(chan *SXIDErrorEvent, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		event := <-monitor.EventChan
		done <- event
		wg.Done()
	}()

	err = monitor.compareBootIDAndEmitHealthyEventIfChanged(initialState, "new-boot-id")
	require.NoError(t, err)

	// wait for the event to be received
	select {
	case receivedEvent := <-done:
		require.NotNil(t, receivedEvent)
		require.False(t, receivedEvent.IsFatal)
		require.True(t, receivedEvent.IsHealthy)
		require.Equal(t, "System reboot detected. BootID changed from old-boot-id to new-boot-id", receivedEvent.Message)
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for bootID change event")
	}

	// verify that the state file has been updated with the new bootID
	updatedState, err := loadState(testStateFilePath)
	require.NoError(t, err)
	require.Equal(t, "new-boot-id", updatedState.BootID)

	wg.Wait()
}

func TestBootIDNoChangeDoesNotEmitEvent(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	initialState := nvSwitchMonitorState{
		LastTimestamp: 12345.6789,
		LastLogLine:   "initial log line",
		BootID:        "current-boot-id",
	}

	err := saveState(testStateFilePath, initialState)
	require.NoError(t, err)

	config := &SxidEventMonitorConfig{
		StateFilePath:                 testStateFilePath,
		PollingIntervalInMilliseconds: 1000,
	}
	monitor, err := NewSxidEventMonitor(config)
	require.NoError(t, err)
	defer monitor.Close()

	eventReceived := make(chan *SXIDErrorEvent, 1)
	go func() {
		event := <-monitor.EventChan
		eventReceived <- event
	}()

	err = monitor.compareBootIDAndEmitHealthyEventIfChanged(initialState, "current-boot-id")
	require.NoError(t, err)

	select {
	case receivedEvent := <-eventReceived:
		t.Fatalf("Expected no event, but received: %+v", receivedEvent)
	case <-time.After(100 * time.Millisecond):
		// success, no event was emitted
	}

	updatedState, err := loadState(testStateFilePath)
	require.NoError(t, err)
	require.Equal(t, "current-boot-id", updatedState.BootID)
}

func TestBootIDInitialization(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	initialState := nvSwitchMonitorState{
		LastTimestamp: 0.0,
		LastLogLine:   "",
		BootID:        "",
	}

	err := saveState(testStateFilePath, initialState)
	require.NoError(t, err)

	config := &SxidEventMonitorConfig{
		StateFilePath:                 testStateFilePath,
		PollingIntervalInMilliseconds: 1000,
	}
	monitor, err := NewSxidEventMonitor(config)
	require.NoError(t, err)
	defer monitor.Close()

	done := make(chan *SXIDErrorEvent, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		event := <-monitor.EventChan
		done <- event
		wg.Done()
	}()

	err = monitor.compareBootIDAndEmitHealthyEventIfChanged(initialState, "new-boot-id")
	require.NoError(t, err)

	select {
	case receivedEvent := <-done:
		require.NotNil(t, receivedEvent)
		require.False(t, receivedEvent.IsFatal)
		require.True(t, receivedEvent.IsHealthy)
		require.Equal(t, "System reboot detected. BootID changed from  to new-boot-id", receivedEvent.Message)
	case <-time.After(1 * time.Second):
		t.Fatal("Timeout waiting for bootID change event")
	}

	updatedState, err := loadState(testStateFilePath)
	require.NoError(t, err)
	require.Equal(t, "new-boot-id", updatedState.BootID)

	wg.Wait()
}

func TestStateVersionMismatch(t *testing.T) {
	testDir := t.TempDir()
	testStateFilePath := filepath.Join(testDir, "state.json")

	initialState := nvSwitchMonitorState{
		Version:       -1, // a non-zero initial version for testing
		LastTimestamp: 12345.6789,
		LastLogLine:   "old version log line",
		BootID:        "old-boot-id",
	}

	err := saveState(testStateFilePath, initialState)
	require.NoError(t, err)

	config := &SxidEventMonitorConfig{
		StateFilePath:                 testStateFilePath,
		PollingIntervalInMilliseconds: 1000,
	}

	monitor, err := NewSxidEventMonitor(config)
	require.NoError(t, err)
	defer monitor.Close()

	// state should be loaded successfully, and version should be updated to the current stateFileVersion
	loadedState, err := loadState(testStateFilePath)
	require.NoError(t, err)
	require.Equal(t, stateFileVersion, loadedState.Version)
	require.Equal(t, "old-boot-id", loadedState.BootID)
	require.Equal(t, "old version log line", loadedState.LastLogLine)
	require.Equal(t, 12345.6789, loadedState.LastTimestamp)

	// case where necessary fields are missing (i.e. incompatible version)
	incompleteState := nvSwitchMonitorState{
		Version:       -1,
		LastTimestamp: 0.0,
		LastLogLine:   "",
		BootID:        "",
	}

	err = saveState(testStateFilePath, incompleteState)
	require.NoError(t, err)

	// expecting a version mismatch error
	_, err = loadState(testStateFilePath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "state file version mismatch")
}

func TestStateFileEmptyOrCorrupted(t *testing.T) {
	testDir := t.TempDir()
	emptyStateFilePath := filepath.Join(testDir, "empty_state.json")
	corruptedStateFilePath := filepath.Join(testDir, "corrupted_state.json")
	nonExistentStateFilePath := filepath.Join(testDir, "non_existent_state.json")

	// case 1: Empty state file
	err := os.WriteFile(emptyStateFilePath, []byte{}, 0600)
	require.NoError(t, err)

	state, err := loadState(emptyStateFilePath)
	require.NoError(t, err, "loadState should not error on empty file")
	require.Equal(t, nvSwitchMonitorState{}, state, "loadState should return zero state for empty file")

	// case 2: Corrupted state file (invalid JSON)
	err = os.WriteFile(corruptedStateFilePath, []byte("this is not json"), 0600)
	require.NoError(t, err)

	state, err = loadState(corruptedStateFilePath)
	require.NoError(t, err, "loadState should not error on corrupted file")
	expectedState := nvSwitchMonitorState{
		Version:       stateFileVersion,
		BootID:        "",
		LastTimestamp: 0.0,
		LastLogLine:   "",
	}
	require.Equal(t, expectedState, state, "loadState should return default state for corrupted file")

	// case 3: Non-existent state file
	state, err = loadState(nonExistentStateFilePath)
	require.NoError(t, err, "loadState should not error on non-existent file")
	require.Equal(t, nvSwitchMonitorState{}, state, "loadState should return zero state for non-existent file")
}
