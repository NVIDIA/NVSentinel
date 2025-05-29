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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// originalFS holds the original fileSystem global to be restored after tests.
// It refers to the FileSystem interface defined in os_fs.go
var originalFS FileSystem

// We also capture originals for the overridable OS helpers so we can restore them
// after each test that uses a mocked file-system.
var (
	originalOsStat      func(string) (os.FileInfo, error)
	originalOsWriteFile func(string, []byte, fs.FileMode) error
	originalOsMkdirAll  func(string, fs.FileMode) error
	originalOsReadFile  func(string) ([]byte, error)
)

// Reset global boot ID state so tests are isolated

// setupMockFS configures the package-level fileSystem variable to use a MockFileSystem.
// It assumes MockFileSystem is defined in os_fs.go and originalFS is used for restoration.
func setupMockFS() *MockFileSystem {
	mockFS := &MockFileSystem{
		Fs: make(fstest.MapFS),
	}

	// Reset global boot ID state so tests are isolated
	storedBootID = ""

	// Swap out the global filesystem implementation
	originalFS = fileSystem
	fileSystem = mockFS

	// Patch stat / write helpers so production code that still uses the
	// os* variables transparently works with our in-memory filesystem.
	originalOsStat = osStat
	osStat = mockFS.Stat

	originalOsWriteFile = osWriteFile
	osWriteFile = func(name string, data []byte, perm fs.FileMode) error {
		mockFS.mu.Lock()
		defer mockFS.mu.Unlock()
		mockFS.Fs[strings.TrimPrefix(name, "/")] = &fstest.MapFile{Data: data, Mode: perm}
		return nil
	}

	originalOsMkdirAll = osMkdirAll
	osMkdirAll = func(path string, perm fs.FileMode) error {
		// Directories are implicit in fstest.MapFS – nothing to do here.
		return nil
	}

	originalOsReadFile = osReadFile
	osReadFile = func(name string) ([]byte, error) {
		// Try full trimmed path first
		trimmed := strings.TrimPrefix(name, "/")
		if data, err := mockFS.ReadFile(trimmed); err == nil {
			return data, nil
		}
		// Fallback to just the base name (helpful when directories were not
		// explicitly created in the MapFS).
		return mockFS.ReadFile(filepath.Base(name))
	}

	return mockFS
}

func teardownMockFS() {
	// Restore globals to their originals so other tests are unaffected
	fileSystem = originalFS
	osStat = originalOsStat
	osWriteFile = originalOsWriteFile
	osMkdirAll = originalOsMkdirAll
	osReadFile = originalOsReadFile
}

func TestNetworkTypesToMonitor(t *testing.T) {
	tests := []struct {
		name               string
		config             *NicMonitorConfig
		expectedMonitorEth bool
		expectedMonitorIB  bool
	}{
		{
			name:               "All",
			config:             &NicMonitorConfig{MonitorNetworkType: MonitorNetworkTypeAll},
			expectedMonitorEth: true,
			expectedMonitorIB:  true,
		},
		{
			name:               "RoCE",
			config:             &NicMonitorConfig{MonitorNetworkType: MonitorNetworkTypeRoCE},
			expectedMonitorEth: true,
			expectedMonitorIB:  true, // IB monitor filters by link_layer internally
		},
		{
			name:               "InfiniBand",
			config:             &NicMonitorConfig{MonitorNetworkType: MonitorNetworkTypeInfiniBand},
			expectedMonitorEth: false,
			expectedMonitorIB:  true,
		},
		{
			name:               "UnknownType",
			config:             &NicMonitorConfig{MonitorNetworkType: "unknown"},
			expectedMonitorEth: true, // Defaults to all
			expectedMonitorIB:  true, // Defaults to all
		},
		{
			name:               "NilConfigObject",
			config:             nil,
			expectedMonitorEth: true, // Defaults to all
			expectedMonitorIB:  true, // Defaults to all
		},
		{
			name:               "EmptyMonitorNetworkType",
			config:             &NicMonitorConfig{MonitorNetworkType: ""},
			expectedMonitorEth: true,
			expectedMonitorIB:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			collector := &NicHealthMonitor{
				monitorConfig: tc.config,
			} // monitorConfig can be nil for tc "NilConfigObject"
			actualMonitorEth, actualMonitorIB := networkTypesToMonitor(collector)
			assert.Equal(t, tc.expectedMonitorEth, actualMonitorEth, "monitorEth mismatch")
			assert.Equal(t, tc.expectedMonitorIB, actualMonitorIB, "monitorIB mismatch")
		})
	}
}

func TestScanAndRegisterNics(t *testing.T) {
	tests := []struct {
		name                 string
		configType           MonitorNetworkType
		ibPathExists         bool
		netPathExists        bool
		expectedMonitorTypes []reflect.Type
	}{
		{
			name:          "All_BothPathsExist",
			configType:    MonitorNetworkTypeAll,
			ibPathExists:  true,
			netPathExists: true,
			expectedMonitorTypes: []reflect.Type{
				reflect.TypeOf(&InfinibandDeviceMonitor{}),
				reflect.TypeOf(&EthernetDeviceMonitor{}),
			},
		},
		{
			name:                 "All_IBPathMissing",
			configType:           MonitorNetworkTypeAll,
			ibPathExists:         false,
			netPathExists:        true,
			expectedMonitorTypes: []reflect.Type{reflect.TypeOf(&EthernetDeviceMonitor{})},
		},
		{
			name:                 "All_NetPathMissing",
			configType:           MonitorNetworkTypeAll,
			ibPathExists:         true,
			netPathExists:        false,
			expectedMonitorTypes: []reflect.Type{reflect.TypeOf(&InfinibandDeviceMonitor{})},
		},
		{
			name:                 "All_BothPathsMissing",
			configType:           MonitorNetworkTypeAll,
			ibPathExists:         false,
			netPathExists:        false,
			expectedMonitorTypes: []reflect.Type{},
		},
		{
			name:          "RoCE_BothPathsExist",
			configType:    MonitorNetworkTypeRoCE,
			ibPathExists:  true,
			netPathExists: true,
			expectedMonitorTypes: []reflect.Type{
				reflect.TypeOf(&InfinibandDeviceMonitor{}),
				reflect.TypeOf(&EthernetDeviceMonitor{}),
			},
		},
		{
			name:                 "InfiniBand_IBPathExists_NetPathIrrelevant",
			configType:           MonitorNetworkTypeInfiniBand,
			ibPathExists:         true,
			netPathExists:        true, // Net path can exist or not, IB monitor should still register
			expectedMonitorTypes: []reflect.Type{reflect.TypeOf(&InfinibandDeviceMonitor{})},
		},
		{
			name:                 "InfiniBand_IBPathMissing",
			configType:           MonitorNetworkTypeInfiniBand,
			ibPathExists:         false,
			netPathExists:        true,
			expectedMonitorTypes: []reflect.Type{},
		},
		{
			name:                 "RoCE_NetPathMissing_IBPathExists", // RoCE needs IB path for IB part of RoCE
			configType:           MonitorNetworkTypeRoCE,
			ibPathExists:         true,
			netPathExists:        false,
			expectedMonitorTypes: []reflect.Type{reflect.TypeOf(&InfinibandDeviceMonitor{})},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockFS := setupMockFS()
			defer teardownMockFS()

			// Setup mock filesystem based on tc.ibPathExists and tc.netPathExists
			// The MockFileSystem in os_fs.go uses strings.TrimPrefix for paths.
			// It doesn't have specific error injection fields like StatErr.
			// Existence is modeled by presence in its Fs map.
			if tc.ibPathExists {
				mockFS.Fs[strings.TrimPrefix(SYS_CLASS_INFINIBAND_PATH, "/")] = &fstest.MapFile{Mode: fs.ModeDir}
			}
			if tc.netPathExists {
				mockFS.Fs[strings.TrimPrefix(SYS_CLASS_NET_PATH, "/")] = &fstest.MapFile{Mode: fs.ModeDir}
			}
			// To simulate os.ErrNotExist for dirExists, ensure the path is NOT in mockFS.Fs

			config := &NicMonitorConfig{MonitorNetworkType: tc.configType}
			collector := &NicHealthMonitor{monitorConfig: config, EventChan: make(chan *[]NicHealthEvent, 5)}

			scanAndRegisterNics(collector)

			assert.Equal(
				t,
				len(tc.expectedMonitorTypes),
				len(collector.Monitors),
				"Number of registered monitors mismatch",
			)

			registeredTypes := make([]reflect.Type, len(collector.Monitors))
			for i, m := range collector.Monitors {
				registeredTypes[i] = reflect.TypeOf(m)
			}

			for _, expectedType := range tc.expectedMonitorTypes {
				assert.Contains(
					t,
					registeredTypes,
					expectedType,
					"Expected monitor type %v not registered",
					expectedType,
				)
			}
		})
	}
}

func TestBootIDLogic(t *testing.T) {
	const testStateFilePath = "/tmp/nic_monitor_test_state.json"
	// Clean up any state file that might be created by tests that use the real filesystem by mistake
	defer os.Remove(testStateFilePath)

	originalFetchCurrentBootID := fetchCurrentBootID
	defer func() { fetchCurrentBootID = originalFetchCurrentBootID }()

	t.Run("NewNic_NoStateFile_InitialBoot", func(t *testing.T) {
		mockFS := setupMockFS()
		defer teardownMockFS()

		fetchCurrentBootID = func() (string, error) { return "boot-id-1", nil }
		// Ensure state file does not exist in mockFS for loadNicMonitorState to get os.ErrNotExist
		// The MockFileSystem in os_fs.go's ReadFile will return error if not found.

		config := &NicMonitorConfig{
			StateFilePath:                 testStateFilePath,
			PollingIntervalInMilliseconds: 10000,
		}
		monitor, err := NewNicHealthMonitor(config)
		require.NoError(t, err)

		assert.True(t, monitor.initialRebootDetected, "Initial reboot should be detected")
		assert.Equal(t, "", monitor.previousBootIDForEvent, "Previous boot ID should be empty")
		assert.Equal(t, "boot-id-1", storedBootID, "Stored boot ID should be current")

		// Verify state was saved to mock filesystem
		stateData, err := mockFS.ReadFile(strings.TrimPrefix(testStateFilePath, "/"))
		require.NoError(t, err, "State file should have been written to mock FS")
		var savedState NicMonitorState
		err = json.Unmarshal(stateData, &savedState)
		require.NoError(t, err)
		assert.Equal(t, nicMonitorStateFileVersion, savedState.Version)
		assert.Equal(t, "boot-id-1", savedState.BootID)
	})

	t.Run("NewNic_StateFileWithOldBootID_RebootDetected", func(t *testing.T) {
		mockFS := setupMockFS()
		defer teardownMockFS()

		fetchCurrentBootID = func() (string, error) { return "boot-id-new", nil }
		initialState := NicMonitorState{Version: 1, BootID: "boot-id-old"}
		initialStateData, _ := json.Marshal(initialState)
		mockFS.Fs[strings.TrimPrefix(testStateFilePath, "/")] = &fstest.MapFile{Data: initialStateData}

		config := &NicMonitorConfig{StateFilePath: testStateFilePath}
		monitor, err := NewNicHealthMonitor(config)
		require.NoError(t, err)

		assert.True(t, monitor.initialRebootDetected, "Initial reboot should be detected")
		assert.Equal(t, "boot-id-old", monitor.previousBootIDForEvent)
		assert.Equal(t, "boot-id-new", storedBootID)

		stateData, err := mockFS.ReadFile(strings.TrimPrefix(testStateFilePath, "/"))
		require.NoError(t, err)
		var savedState NicMonitorState
		err = json.Unmarshal(stateData, &savedState)
		require.NoError(t, err)
		assert.Equal(t, "boot-id-new", savedState.BootID)
	})

	t.Run("NewNic_StateFileWithCurrentBootID_NoReboot", func(t *testing.T) {
		mockFS := setupMockFS()
		defer teardownMockFS()

		fetchCurrentBootID = func() (string, error) { return "boot-id-current", nil }
		initialState := NicMonitorState{Version: 1, BootID: "boot-id-current"}
		initialStateData, _ := json.Marshal(initialState)
		mockFS.Fs[strings.TrimPrefix(testStateFilePath, "/")] = &fstest.MapFile{Data: initialStateData}

		config := &NicMonitorConfig{StateFilePath: testStateFilePath}
		monitor, err := NewNicHealthMonitor(config)
		require.NoError(t, err)

		assert.False(t, monitor.initialRebootDetected, "Initial reboot should NOT be detected")
		assert.Equal(t, "boot-id-current", storedBootID)
		// Verify state file is re-saved with the current ID (even if same)
		stateData, err := mockFS.ReadFile(strings.TrimPrefix(testStateFilePath, "/"))
		require.NoError(t, err)
		var savedState NicMonitorState
		err = json.Unmarshal(stateData, &savedState)
		require.NoError(t, err)
		assert.Equal(t, "boot-id-current", savedState.BootID)
	})

	t.Run("NewNic_FetchBootIDError_UsesOldStateBootID", func(t *testing.T) {
		mockFS := setupMockFS()
		defer teardownMockFS()

		fetchCurrentBootID = func() (string, error) { return "", errors.New("failed to fetch boot id") }
		initialState := NicMonitorState{Version: 1, BootID: "boot-id-from-state"}
		initialStateData, _ := json.Marshal(initialState)
		mockFS.Fs[strings.TrimPrefix(testStateFilePath, "/")] = &fstest.MapFile{Data: initialStateData}

		config := &NicMonitorConfig{StateFilePath: testStateFilePath}
		monitor, err := NewNicHealthMonitor(config)
		require.NoError(t, err)

		// Since fetch failed, the monitor cannot determine whether a reboot occurred, so
		// initialRebootDetected should be false. The on-disk state should remain unchanged.
		assert.False(t, monitor.initialRebootDetected, "Reboot should NOT be detected when boot ID fetch fails")
		assert.Equal(t, "", monitor.previousBootIDForEvent)
		assert.Equal(t, "boot-id-from-state", storedBootID, "storedBootID should remain from state file")

		stateData, err := mockFS.ReadFile(strings.TrimPrefix(testStateFilePath, "/"))
		require.NoError(t, err, "State file should still exist")
		var savedState NicMonitorState
		err = json.Unmarshal(stateData, &savedState)
		require.NoError(t, err)
		assert.Equal(t, "boot-id-from-state", savedState.BootID, "State file should be unchanged when fetch fails")
	})

	t.Run("Run_DetectsBootIDChange_EmitsEvent_SavesState", func(t *testing.T) {
		mockFS := setupMockFS()
		defer teardownMockFS()

		// Setup initial state for NewNicHealthMonitor
		currentBootID := "initial-boot-id"
		fetchCurrentBootID = func() (string, error) { return currentBootID, nil }
		initialState := NicMonitorState{Version: 1, BootID: "initial-boot-id"}
		initialStateData, _ := json.Marshal(initialState)
		mockFS.Fs[strings.TrimPrefix(testStateFilePath, "/")] = &fstest.MapFile{Data: initialStateData}

		// Ensure monitors are registered for event emission
		mockFS.Fs[strings.TrimPrefix(SYS_CLASS_NET_PATH, "/")] = &fstest.MapFile{Mode: fs.ModeDir}
		mockFS.Fs[strings.TrimPrefix(SYS_CLASS_INFINIBAND_PATH, "/")] = &fstest.MapFile{Mode: fs.ModeDir}

		config := &NicMonitorConfig{
			StateFilePath:                 testStateFilePath,
			PollingIntervalInMilliseconds: 20, // Fast polling for test
			MonitorNetworkType:            MonitorNetworkTypeAll,
			MaxRetryDurationForDownDetectedNICInMilliseconds: 100,
			RetryIntervalForDownDetectedNICInMilliseconds:    10,
		}
		monitor, err := NewNicHealthMonitor(config)
		require.NoError(t, err)
		require.False(t, monitor.initialRebootDetected, "No initial reboot expected")
		require.Equal(t, "initial-boot-id", storedBootID)

		// Replace EventChan with a buffered version to avoid blocking when the
		// reboot detection emits events before we can receive them in this
		// test goroutine.
		bufferedChan := make(chan *[]NicHealthEvent, 4)
		monitor.EventChan = bufferedChan

		// Simulate boot ID change by updating fetchCurrentBootID and then invoking
		// the reboot-detection logic directly instead of spinning up the infinite
		// Run() loop – this keeps the test single-threaded and race-free.
		newBootID := "new-boot-id"
		fetchCurrentBootID = func() (string, error) { return newBootID, nil }

		// Call the reboot detector manually.
		monitor.detectAndHandleReboot()

		// Read events emitted.
		select {
		case events := <-bufferedChan:
			require.NotNil(t, events)
			assert.Len(t, *events, 2, "Should receive two reboot events (one per monitor type)")
			for _, event := range *events {
				assert.True(t, event.IsHealthyEvent)
				assert.Contains(t, event.Message, "System reboot detected")
				assert.Contains(t, event.Message, "initial-boot-id")
				assert.Contains(t, event.Message, "new-boot-id")
			}

			stateData, err := mockFS.ReadFile(strings.TrimPrefix(testStateFilePath, "/"))
			require.NoError(t, err)
			var savedState NicMonitorState
			require.NoError(t, json.Unmarshal(stateData, &savedState))
			assert.Equal(t, newBootID, savedState.BootID)
			assert.Equal(t, newBootID, storedBootID)

		case <-time.After(time.Second):
			t.Fatal("Timed out waiting for reboot event after manual boot ID change")
		}
	})
}

// TestLoadNicMonitorState verifies the behavior of loadNicMonitorState.
func TestLoadNicMonitorState(t *testing.T) {
	const testStateFilePath = "/tmp/nic_monitor_load_test_state.json"
	defer os.Remove(testStateFilePath)

	originalOsReadFile := osReadFile // This is the package var from nic_monitor.go
	defer func() { osReadFile = originalOsReadFile }()

	tests := []struct {
		name           string
		fileData       []byte // Data for osReadFile mock to return
		readFileErr    error  // Error for osReadFile mock to return
		expectedState  NicMonitorState
		expectedError  bool
		expectedBootID string
	}{
		{
			name:           "FileExistsAndValid",
			fileData:       []byte(`{"version":1,"boot_id":"test-boot-123"}`),
			readFileErr:    nil,
			expectedState:  NicMonitorState{Version: 1, BootID: "test-boot-123"},
			expectedBootID: "test-boot-123",
		},
		{
			name:           "FileDoesNotExist",
			fileData:       nil,
			readFileErr:    os.ErrNotExist,
			expectedState:  NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""},
			expectedBootID: "",
		},
		{
			name:           "FileExistsButEmpty",
			fileData:       []byte{},
			readFileErr:    nil,
			expectedState:  NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""},
			expectedBootID: "",
		},
		{
			name:           "FileCorruptedJson",
			fileData:       []byte(`{"version":1,`), // Invalid JSON
			readFileErr:    nil,
			expectedState:  NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""},
			expectedBootID: "",
		},
		{
			name:           "VersionMismatch",
			fileData:       []byte(`{"version":0,"boot_id":"test-boot-456"}`),
			readFileErr:    nil,
			expectedState:  NicMonitorState{Version: nicMonitorStateFileVersion, BootID: ""},
			expectedBootID: "",
		},
		{
			name:           "OtherReadFileError",
			fileData:       nil,
			readFileErr:    errors.New("disk read error"),
			expectedState:  NicMonitorState{},
			expectedError:  true,
			expectedBootID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Mock osReadFile directly for this test
			osReadFile = func(name string) ([]byte, error) {
				if name == testStateFilePath {
					return tc.fileData, tc.readFileErr
				}
				// Fallback for any other unexpected path, though not expected for this test
				return nil, fmt.Errorf("osReadFile mock called with unexpected path: %s", name)
			}

			state, err := loadNicMonitorState(testStateFilePath)

			if tc.expectedError {
				require.Error(t, err)
				// It's good practice to also check if the error is the one expected if possible,
				// but for now, just checking for an error is fine based on existing structure.
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.expectedState.Version, state.Version, "State version mismatch")
			assert.Equal(t, tc.expectedBootID, state.BootID, "State BootID mismatch")
		})
	}
}

func TestSaveNicMonitorState(t *testing.T) {
	const testStateFilePath = "/tmp/nic_monitor_save_test_state.json"
	const testStateDir = "/tmp"
	defer os.Remove(testStateFilePath) // General cleanup for the test file

	stateToSave := NicMonitorState{Version: nicMonitorStateFileVersion, BootID: "save-test-boot-id"}

	// originalOsXXX vars are restored at the end of TestSaveNicMonitorState function execution
	originalOsMkdirAll := osMkdirAll
	originalOsWriteFile := osWriteFile
	defer func() {
		osMkdirAll = originalOsMkdirAll
		osWriteFile = originalOsWriteFile
	}()

	t.Run("SuccessfulSave", func(t *testing.T) {
		// No mockFS setup needed here as os funcs are directly mocked
		var mkdirCalledPath string
		var writeFileCalledPath string
		var writtenData []byte

		osMkdirAll = func(path string, perm fs.FileMode) error {
			mkdirCalledPath = path
			return nil
		}
		osWriteFile = func(name string, data []byte, perm fs.FileMode) error {
			writeFileCalledPath = name
			writtenData = data
			return nil
		}

		err := saveNicMonitorState(testStateFilePath, stateToSave)
		require.NoError(t, err)

		assert.Equal(t, testStateDir, mkdirCalledPath)
		assert.Equal(t, testStateFilePath, writeFileCalledPath)
		expectedData, _ := json.Marshal(stateToSave)
		assert.JSONEq(t, string(expectedData), string(writtenData))
	})

	t.Run("MkdirAllError", func(t *testing.T) {
		// No mockFS setup needed here
		expectedErr := errors.New("mkdir failed")
		osMkdirAll = func(path string, perm fs.FileMode) error {
			return expectedErr
		}
		// Reset osWriteFile to a non-nil default or a specific mock if its call is possible
		// For this specific subtest, osWriteFile shouldn't be reached if MkdirAll fails.
		// However, to be safe from previous subtest states if tests run in parallel (not default) or if logic changes:
		tempOsWriteFile := osWriteFile                                                      // Save current (possibly mocked by other test)
		osWriteFile = func(name string, data []byte, perm fs.FileMode) error { return nil } // Benign mock
		defer func() { osWriteFile = tempOsWriteFile }()                                    // Restore after this subtest

		err := saveNicMonitorState(testStateFilePath, stateToSave)
		require.Error(t, err)
		assert.True(t, errors.Is(err, expectedErr) || strings.Contains(err.Error(), expectedErr.Error()))
	})

	t.Run("WriteFileError", func(t *testing.T) {
		// No mockFS setup needed here
		expectedErr := errors.New("write failed")
		osMkdirAll = func(path string, perm fs.FileMode) error { return nil } // Mkdir succeeds
		osWriteFile = func(name string, data []byte, perm fs.FileMode) error {
			return expectedErr
		}

		err := saveNicMonitorState(testStateFilePath, stateToSave)
		require.Error(t, err)
		assert.True(t, errors.Is(err, expectedErr) || strings.Contains(err.Error(), expectedErr.Error()))
	})
}
