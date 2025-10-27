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
package syslogmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/sxid"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
)

// NewSyslogMonitor creates a new SyslogMonitor instance
func NewSyslogMonitor(nodeName string, checks []CheckDefinition, pcClient pb.PlatformConnectorClient,
	defaultAgentName string, defaultComponentClass string,
	pollingInterval string, stateFilePath string, xidAnalyserEndpoint string) (*SyslogMonitor, error) {
	return NewSyslogMonitorWithFactory(nodeName, checks, pcClient, defaultAgentName,
		defaultComponentClass, pollingInterval, stateFilePath, GetDefaultJournalFactory(),
		xidAnalyserEndpoint,
	)
}

// NewSyslogMonitorWithFactory creates a new SyslogMonitor instance with a specific journal factory
//
//nolint:cyclop
func NewSyslogMonitorWithFactory(nodeName string, checks []CheckDefinition, pcClient pb.PlatformConnectorClient,
	defaultAgentName string, defaultComponentClass string, pollingInterval string,
	stateFilePath string, journalFactory JournalFactory, xidAnalyserEndpoint string) (*SyslogMonitor, error) {
	// Load state from file
	state, err := loadState(stateFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	// Get current boot ID
	currentBootID, err := fetchCurrentBootID()
	if err != nil {
		slog.Warn("Failed to get current boot ID", "error", err)

		currentBootID = ""
	}

	sm := &SyslogMonitor{
		nodeName:              nodeName,
		checks:                checks,
		pcClient:              pcClient,
		defaultAgentName:      defaultAgentName,
		defaultComponentClass: defaultComponentClass,
		pollingInterval:       pollingInterval,
		checkLastCursors:      state.CheckLastCursors,
		journalFactory:        journalFactory,
		currentBootID:         currentBootID,
		stateFilePath:         stateFilePath,
		checkToHandlerMap:     make(map[string]types.Handler),
		xidAnalyserEndpoint:   xidAnalyserEndpoint,
	}

	for _, check := range checks {
		switch check.Name {
		case XIDErrorCheck:
			xidHandler, err := xid.NewXIDHandler(nodeName,
				defaultAgentName, defaultComponentClass, check.Name, xidAnalyserEndpoint)
			if err != nil {
				slog.Error("Error initializing XID handler", "error", err.Error())
				return nil, fmt.Errorf("failed to initialize XID handler: %w", err)
			}

			sm.checkToHandlerMap[check.Name] = xidHandler

		case SXIDErrorCheck:
			sxidHandler, err := sxid.NewSXIDHandler(
				nodeName, defaultAgentName, defaultComponentClass, check.Name)
			if err != nil {
				slog.Error("Error initializing SXID handler", "error", err.Error())
				return nil, fmt.Errorf("failed to initialize SXID handler: %w", err)
			}

			sm.checkToHandlerMap[check.Name] = sxidHandler

		default:
			slog.Error("Unsupported check", "check", check.Name)
		}
	}

	// Handle boot ID changes (system reboot detection)
	if err := sm.handleBootIDChange(state.BootID, currentBootID); err != nil {
		return nil, fmt.Errorf("failed to handle boot ID change: %w", err)
	}

	slog.Info("SyslogMonitor initialized with persistent state. Each check will resume from last processed cursor.")

	return sm, nil
}

// Run executes all configured checks
func (sm *SyslogMonitor) Run() error {
	var jointError error = nil

	for _, check := range sm.checks {
		err := sm.executeCheck(check)
		if err != nil {
			slog.Error("Check '%s' failed during execution: %v", check.Name, err)
			jointError = errors.Join(jointError, err)
		}
	}

	if jointError != nil {
		return jointError
	}

	slog.Info("Syslog monitor run cycle completed successfully.")

	return nil
}

// saveState saves the monitor state to a file
func saveState(stateFilePath string, state syslogMonitorState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal syslog monitor state: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(stateFilePath), 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	if err := os.WriteFile(stateFilePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write state to file: %w", err)
	}

	return nil
}

// loadState loads the monitor state from a file
//
//nolint:cyclop,gocognit // TODO
func loadState(stateFilePath string) (syslogMonitorState, error) {
	var state syslogMonitorState

	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return default state for first run
			return syslogMonitorState{
				Version:          stateFileVersion,
				BootID:           "",
				CheckLastCursors: make(map[string]string),
			}, nil
		}

		return state, fmt.Errorf("failed to read state from file: %w", err)
	}

	// Check if file is empty
	if len(data) == 0 {
		slog.Warn("State file exists but is empty, treating as non-existent",
			"stateFile", stateFilePath)

		return syslogMonitorState{
			Version:          stateFileVersion,
			BootID:           "",
			CheckLastCursors: make(map[string]string),
		}, nil
	}

	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("State file is corrupted, resetting to default",
			"stateFile", stateFilePath,
			"error", err)

		return syslogMonitorState{
			Version:          stateFileVersion,
			BootID:           "",
			CheckLastCursors: make(map[string]string),
		}, nil
	}

	if state.Version != 0 && state.Version != stateFileVersion {
		if verifyStateFields(state) {
			slog.Info("State file version mismatch but compatible",
				"expected", stateFileVersion,
				"actual", state.Version)
			// update the state version to latest current version
			state.Version = stateFileVersion

			if err := saveState(stateFilePath, state); err != nil {
				return state, fmt.Errorf("failed to save updated state: %w", err)
			}

			return state, nil
		}

		return state, fmt.Errorf("state file version mismatch: expected %d, got %d", stateFileVersion, state.Version)
	}

	// Ensure maps are not nil
	if state.CheckLastCursors == nil {
		state.CheckLastCursors = make(map[string]string)
	}

	return state, nil
}

// verifyStateFields verifies if necessary fields for current state version are present
func verifyStateFields(state syslogMonitorState) bool {
	// For syslog monitor, we mainly need the CheckLastCursors map to exist
	return state.CheckLastCursors != nil
}

// fetchCurrentBootID returns the current system boot ID
func fetchCurrentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("failed to read boot_id: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// handleBootIDChange handles system reboot detection and cursor reset
func (sm *SyslogMonitor) handleBootIDChange(oldBootID, newBootID string) error {
	if oldBootID != newBootID {
		slog.Info("Detected bootID change. Old bootID: %s, New bootID: %s", oldBootID, newBootID)

		// Clear all cursors on reboot since journal cursors become invalid
		for checkName := range sm.checkLastCursors {
			delete(sm.checkLastCursors, checkName)
		}

		// Save updated state
		state := syslogMonitorState{
			Version:          stateFileVersion,
			BootID:           newBootID,
			CheckLastCursors: sm.checkLastCursors,
		}

		if err := saveState(sm.stateFilePath, state); err != nil {
			return fmt.Errorf("failed to save state after boot ID change: %w", err)
		}

		slog.Info("Cleared all cursors due to system reboot")

		// Publish healthy events for all checks after a reboot
		if sm.pcClient != nil {
			for _, check := range sm.checks {
				message := "No Health Failures"
				errRes := types.ErrorResolution{
					RecommendedAction: pb.RecommendedAction_NONE,
				}
				healthEvents := sm.prepareHealthEventWithAction(check, message, true, errRes)
				sm.sendHealthEventWithRetry(healthEvents, 5, 2*time.Second)
				slog.Info("Published healthy event after system reboot", "check", check.Name)
			}
		} else {
			slog.Warn("Platform connector client is nil, cannot send healthy events after reboot")
		}
	}

	return nil
}

// saveCurrentState saves the current state to the state file
func (sm *SyslogMonitor) saveCurrentState() error {
	state := syslogMonitorState{
		Version:          stateFileVersion,
		BootID:           sm.currentBootID,
		CheckLastCursors: sm.checkLastCursors,
	}

	return saveState(sm.stateFilePath, state)
}

// executeCheck performs a single log check based on the provided definition
func (sm *SyslogMonitor) executeCheck(check CheckDefinition) error {
	slog.Info("Executing check", "check", check.Name)

	journal, err := sm.openJournal(check)
	if err != nil {
		return fmt.Errorf("failed to open journal for check %s: %w", check.Name, err)
	}

	defer func() {
		if cerr := journal.Close(); cerr != nil {
			slog.Warn("Check '%s': error closing journal: %v", check.Name, cerr)
		}
	}()

	if err := sm.configureTagFilters(journal, check); err != nil {
		return fmt.Errorf("failed to configure tag filters for check %s: %w", check.Name, err)
	}

	err = sm.processJournalEntries(journal, check)
	if err != nil {
		return fmt.Errorf("failed to process journal entries for check %s: %w", check.Name, err)
	}

	// Save state after successfully processing journal entries
	if err := sm.saveCurrentState(); err != nil {
		slog.Warn("Failed to save state after processing check '%s': %v", check.Name, err)
	}

	return nil
}

// validateJournalPath validates the journal path on the filesystem
func (sm *SyslogMonitor) validateJournalPath(check CheckDefinition) error {
	fileInfo, err := os.Stat(check.JournalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("check '%s': journal path does not exist: %s", check.Name, check.JournalPath)
		}

		return fmt.Errorf("check '%s': error accessing journal path %s: %w", check.Name, check.JournalPath, err)
	}

	if !fileInfo.IsDir() {
		return fmt.Errorf("check '%s': journal path is not a directory: %s", check.Name, check.JournalPath)
	}

	return nil
}

// openJournal opens the systemd journal with the specified path
func (sm *SyslogMonitor) openJournal(check CheckDefinition) (Journal, error) {
	//nolint:nestif
	if check.JournalPath != "" {
		slog.Info("Check '%s': Verifying journal path: %s", check.Name, check.JournalPath)

		// Only validate path on filesystem for real journal factories
		if sm.journalFactory.RequiresFileSystemCheck() {
			if err := sm.validateJournalPath(check); err != nil {
				return nil, fmt.Errorf("journal path validation failed for check %s: %w", check.Name, err)
			}
		}

		slog.Info("Check '%s': Opening journal at path: %s", check.Name, check.JournalPath)

		journal, err := sm.journalFactory.NewJournalFromDir(check.JournalPath)
		if err != nil {
			return nil, fmt.Errorf("check '%s': failed to open journal from dir %s: %w", check.Name, check.JournalPath, err)
		}

		return journal, nil
	} else {
		return nil, fmt.Errorf("check '%s': journal path is empty. Path-specific journal expected for checks", check.Name)
	}
}

// configureBootFilter sets up the boot filter for the journal
func (sm *SyslogMonitor) configureBootFilter(journal Journal, checkName string) error {
	bootID := sm.getCurrentBootID()
	if bootID != "" {
		matchExpr := FieldBootID + "=" + bootID

		slog.Info("Check '%s': Applying boot filter: %s", checkName, matchExpr)

		if err := journal.AddMatch(matchExpr); err != nil {
			return fmt.Errorf("check '%s': failed to add boot ID match ('%s'): %w", checkName, matchExpr, err)
		}
	} else {
		slog.Warn("Could not determine current boot ID, boot filter not applied", "check", checkName)
	}

	return nil
}

// configureTagFilters sets up the tag-based filters for the journal
//
//nolint:gocognit,cyclop
func (sm *SyslogMonitor) configureTagFilters(journal Journal, check CheckDefinition) error {
	for _, tag := range check.Tags {
		trimmedTag := strings.TrimSpace(tag)
		if trimmedTag == "" {
			continue
		}

		switch trimmedTag {
		case "-k", "--dmesg":
			// Facility 0 is typically KERNEL messages.
			matchExpr := FieldSyslogFacility + "=0"

			slog.Info("Adding kernel log filter",
				"check", check.Name,
				"tag", trimmedTag,
				"match", matchExpr)

			if err := journal.AddMatch(matchExpr); err != nil {
				return fmt.Errorf("check '%s': failed to add kernel match ('%s'): %w", check.Name, matchExpr, err)
			}
		case "-b", "--boot":
			slog.Info("Processing explicit boot tag",
				"check", check.Name,
				"tag", trimmedTag)
			// configureBootFilter is already called if check.Boot is true.
			// Calling it again here due to an explicit tag is generally harmless if configureBootFilter is idempotent.
			if err := sm.configureBootFilter(journal, check.Name); err != nil {
				return err // Error message from configureBootFilter should be sufficient
			}
		default:
			if strings.HasPrefix(trimmedTag, "-u ") || strings.HasPrefix(trimmedTag, "--unit ") { //nolint:nestif // TODO
				var unitName string
				if strings.HasPrefix(trimmedTag, "-u ") {
					unitName = strings.TrimSpace(strings.TrimPrefix(trimmedTag, "-u "))
				} else { // Must be --unit due to the check above
					unitName = strings.TrimSpace(strings.TrimPrefix(trimmedTag, "--unit "))
				}

				if unitName != "" {
					matchExpr := FieldSystemdUnit + "=" + unitName

					slog.Info("Adding unit filter",
						"check", check.Name,
						"tag", trimmedTag,
						"match", matchExpr)

					if err := journal.AddMatch(matchExpr); err != nil {
						return fmt.Errorf("check '%s': failed to add unit match for '%s' (using expression '%s'): %w",
							check.Name, unitName, matchExpr, err)
					}
				} else {
					slog.Warn("Tag for unit filtering resulted in empty unit name",
						"check", check.Name,
						"tag", trimmedTag)
				}
			} else {
				slog.Info("Check '%s': Ignoring unrecognized tag in 'configureTagFilters': '%s'", check.Name, trimmedTag)
			}
		}
	}

	return nil
}

// processJournalEntries reads and processes journal entries
//
//nolint:gocognit,cyclop
func (sm *SyslogMonitor) processJournalEntries(journal Journal, check CheckDefinition) error {
	// currentEntryCursor will store the cursor of the entry currently being processed or just processed.
	// sm.checkLastCursors[checkName] will store the cursor to resume from on the NEXT run.
	lastKnownCursor, hasLastCursor := sm.checkLastCursors[check.Name]

	bootID, err := journal.GetBootID()
	if err != nil {
		slog.Warn("Failed to get boot ID", "check", check.Name, "error", err)
	}

	slog.Info("Boot ID for check", "check", check.Name, "bootID", bootID)
	// This block handles:
	// 1. Non-boot checks on their first run (hasLastCursor == false)
	// 2. All checks (boot or non-boot) on subsequent runs (hasLastCursor == true)
	//nolint:nestif // TODO
	if !hasLastCursor { // This implies !check.Boot due to the block above
		slog.Info("No last known cursor, seeking to journal tail",
			"check", check.Name)

		if err := journal.SeekTail(); err != nil {
			return fmt.Errorf("check '%s': failed to seek to journal tail for initialization: %w", check.Name, err)
		}

		count, errPrev := journal.Previous()
		if errPrev != nil && !errors.Is(errPrev, io.EOF) {
			return fmt.Errorf("seek previous: %w", errPrev)
		}

		if count == 0 { // journal is empty
			slog.Info("Journal is empty, nothing to do", "check", check.Name)
			return nil
		}

		cursor, err := journal.GetCursor()
		if err != nil {
			if strings.Contains(err.Error(), "cannot assign requested address") {
				slog.Info("No cursor available, journal empty", "check", check.Name)
				return nil
			}

			return fmt.Errorf("get cursor: %w", err)
		}

		slog.Info("Check '%s': Initialized. Journal processing will start from entries"+
			" after cursor '%s' on the next run.", check.Name, cursor)

		sm.checkLastCursors[check.Name] = cursor

		return nil // No entries processed on this initialization run.
	}

	// If we are here, hasLastCursor is true.

	slog.Info("Resuming from last known cursor",
		"check", check.Name,
		"cursor", lastKnownCursor)

	if err := journal.SeekCursor(lastKnownCursor); err != nil {
		slog.Warn("Failed to seek to last known cursor, re-initializing",
			"check", check.Name,
			"cursor", lastKnownCursor,
			"error", err)

		if errSeekTail := journal.SeekTail(); errSeekTail != nil {
			return fmt.Errorf("check '%s': failed to seek to journal tail during re-initialization after SeekCursor error: %w",
				check.Name, errSeekTail)
		}

		tailCursor, errGetCursor := journal.GetCursor()
		if errGetCursor != nil {
			return fmt.Errorf("check '%s': failed to get cursor at journal tail during re-initialization: %w",
				check.Name, errGetCursor)
		}

		slog.Info("Re-initialized journal processing",
			"check", check.Name,
			"cursor", tailCursor)

		sm.checkLastCursors[check.Name] = tailCursor

		return nil // No entries processed on this re-initialization run.
	}

	// Successfully sought to lastKnownCursor. Now advance to the *next* entry.
	// This is crucial: we process entries *after* the lastKnownCursor.
	advanced, nextErr := journal.Next()
	if nextErr != nil && !errors.Is(nextErr, io.EOF) {
		return fmt.Errorf("check '%s': error advancing from resumed cursor '%s': %w", check.Name, lastKnownCursor, nextErr)
	}

	if nextErr == io.EOF || advanced == 0 { //nolint:errorlint // TODO
		slog.Info("No new entries since last cursor",
			"check", check.Name,
			"cursor", lastKnownCursor)
		// sm.checkLastCursors[checkName] is already lastKnownCursor, which is correct for the next run.
		return nil
	}

	// Journal cursor is now positioned at the first new entry to process.
	for {
		currentEntryCursor, err := journal.GetCursor() // Cursor of the entry we are about to process
		if err != nil {
			slog.Warn("Failed to get cursor for current entry, attempting to advance",
				"check", check.Name,
				"error", err,
				"lastStoredCursor", sm.checkLastCursors[check.Name])

			advancedNext, advErr := journal.Next()
			if advErr == io.EOF || advancedNext == 0 { //nolint:errorlint // TODO
				slog.Info("Reached end of journal while recovering from GetCursor error",
					"check", check.Name,
					"nextCursor", sm.checkLastCursors[check.Name])

				break
			}

			if advErr != nil {
				slog.Error("Error advancing journal after GetCursor error, stopping",
					"check", check.Name,
					"error", advErr,
					"nextCursor", sm.checkLastCursors[check.Name])

				return fmt.Errorf("error advancing after GetCursor error for check '%s' (last stored cursor for next run %s): %w",
					check.Name, sm.checkLastCursors[check.Name], advErr)
			}

			continue // Skip to the next iteration
		}

		message, err := sm.getJournalMessage(journal, check.Name)
		if err != nil {
			slog.Warn("Failed to get journal message, skipping entry",
				"check", check.Name,
				"cursor", currentEntryCursor,
				"error", err,
				"nextCursor", sm.checkLastCursors[check.Name])

			advancedNext, advErr := journal.Next()

			if advErr == io.EOF || advancedNext == 0 { //nolint:errorlint // TODO
				slog.Info("Reached end of journal while recovering from message error",
					"check", check.Name,
					"entryCursor", currentEntryCursor,
					"nextCursor", sm.checkLastCursors[check.Name])

				break
			} else if advErr != nil {
				slog.Error("Error advancing journal after message error, stopping",
					"check", check.Name,
					"entryCursor", currentEntryCursor,
					"error", advErr,
					"nextCursor", sm.checkLastCursors[check.Name])

				return fmt.Errorf("error advancing after getJournalMessage for check '%s' (entry cursor %s, "+
					"last stored cursor for next run %s): %v",
					check.Name, currentEntryCursor, sm.checkLastCursors[check.Name], advErr)
			}

			continue
		}

		if message == "" {
			// Successfully read an empty message. This entry is considered processed.
			sm.checkLastCursors[check.Name] = currentEntryCursor // Update cursor for the next run
			slog.Info("Check, read empty message", "name", check.Name,
				"message", message,
				"cursor", currentEntryCursor)
		} else {
			err = sm.handleSingleLine(check, message)
			if err != nil {
				continue
			}
			// This entry (matched or not) is considered processed.
			sm.checkLastCursors[check.Name] = currentEntryCursor // Update cursor for the next run
			slog.Info("Check, considered processed", "name", check.Name,
				"message", message,
				"cursor", currentEntryCursor)
		}

		advancedNext, advErr := journal.Next()
		if advErr == io.EOF || advancedNext == 0 { //nolint:errorlint // TODO
			slog.Info("Check, no more", "name", check.Name, "cursor", currentEntryCursor)
			// sm.checkLastCursors[checkName] is already set to currentEntryCursor.
			break
		}

		if advErr != nil {
			// Error advancing. currentEntryCursor was the last successfully processed one.
			slog.Error("Error reading next journal entry, stopping",
				"check", check.Name,
				"cursor", currentEntryCursor,
				"error", advErr)

			return fmt.Errorf("check '%s': error reading next journal entry after cursor %s: %w",
				check.Name, currentEntryCursor, advErr)
		}
	}

	finalCursor := sm.checkLastCursors[check.Name] // Should always exist if we passed initialization.
	slog.Info("Finished processing journal entries",
		"check", check.Name,
		"nextCursor", finalCursor)

	return nil
}

// getJournalMessage attempts to read a message from the journal with retry logic
func (sm *SyslogMonitor) getJournalMessage(journal Journal, checkName string) (string, error) {
	var message string

	var err error

	maxRetries := 3
	retryDelay := 100 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		// Try to read the message
		message, err = journal.GetData(FieldMessage)
		if err == nil {
			return message, nil
		}

		// If it's not a retryable error, return immediately
		if !isRetryableJournalError(err) {
			return "", fmt.Errorf("non-retryable error reading journal message for check %s: %w", checkName, err)
		}

		// Log retry attempt
		if i < maxRetries-1 {
			slog.Debug("Retrying journal message read",
				"check", checkName,
				"attempt", i+1,
				"maxRetries", maxRetries,
				"error", err)
			time.Sleep(retryDelay)
		}
	}

	return "", fmt.Errorf("failed to read journal message after %d attempts: %w", maxRetries, err)
}

// isRetryableJournalError determines if a journal error is retryable
func isRetryableJournalError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()

	return strings.Contains(errStr, "cannot assign requested address") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "resource temporarily unavailable") ||
		strings.Contains(errStr, "no such file or directory") ||
		strings.Contains(errStr, "permission denied")
}

// getCurrentBootID returns the current system boot ID
func (sm *SyslogMonitor) getCurrentBootID() string {
	journal, err := sm.journalFactory.NewJournal()
	if err != nil {
		slog.Warn("Failed to open system journal for boot ID", "error", err)
		return ""
	}

	defer func() {
		if cerr := journal.Close(); cerr != nil {
			slog.Warn("Error closing system journal after getting boot ID", "error", cerr)
		}
	}()

	bootID, err := journal.GetBootID()
	if err != nil {
		slog.Warn("Failed to get boot ID", "error", err)
		return ""
	}

	return bootID
}

// prepareHealthEventWithAction creates a health event with an explicit RecommendedAction
func (sm *SyslogMonitor) prepareHealthEventWithAction(
	check CheckDefinition, message string, isHealthy bool, errRes types.ErrorResolution) *pb.HealthEvents {
	slog.Info("Preparing health event with override action",
		"check", check.Name,
		"message", message,
		"healthy", isHealthy,
		"fatal", false,
		"action", errRes.RecommendedAction)

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              sm.defaultAgentName,
		CheckName:          check.Name,
		ComponentClass:     sm.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		Message:            message,
		IsFatal:            false,
		IsHealthy:          isHealthy,
		NodeName:           sm.nodeName,
		RecommendedAction:  errRes.RecommendedAction,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}

// sendHealthEventWithRetry sends health events to platform connector with retry logic
func (sm *SyslogMonitor) sendHealthEventWithRetry(healthEvents *pb.HealthEvents,
	maxRetries int, retryDelay time.Duration) {
	if sm.pcClient == nil {
		slog.Error("PlatformConnectorClient is nil, cannot send health event")
		return
	}

	slog.Info("Attempting to send health event", "events", healthEvents)

	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: retryDelay,
		Factor:   1.5,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := sm.pcClient.HealthEventOccurredV1(context.Background(), healthEvents)
		if err == nil {
			slog.Info("Successfully sent health events", "events", healthEvents)
			return true, nil
		}

		if isRetryableError(err) {
			slog.Warn("Retryable error sending health event, will retry", "error", err)
			return false, nil
		}

		slog.Error("Non-retryable error sending health event", "error", err)

		return false, fmt.Errorf("non-retryable error sending health event: %w", err)
	})

	if err != nil {
		slog.Error("All retry attempts to send health event failed", "error", err)
	}
}

// isRetryableError determines if an error is retryable
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}

	if s, ok := status.FromError(err); ok {
		if s.Code() == codes.Unavailable || s.Code() == codes.DeadlineExceeded {
			return true
		}
	}

	if _, ok := err.(interface{ Temporary() bool }); ok {
		return true
	}

	if err == io.EOF || strings.Contains(err.Error(), "connection reset by peer") || //nolint:errorlint // TODO
		strings.Contains(err.Error(), "broken pipe") {
		return true
	}

	return false
}

func (sm *SyslogMonitor) handleSingleLine(check CheckDefinition, lineToEvaluate string) error {
	if handler, ok := sm.checkToHandlerMap[check.Name]; ok {
		healthEvents, err := handler.ProcessLine(lineToEvaluate)
		if err != nil {
			return fmt.Errorf("error processing line %s: %w", lineToEvaluate, err)
		}

		if healthEvents != nil {
			sm.sendHealthEventWithRetry(healthEvents, 5, 2*time.Second)
		}
	}

	return nil
}
