// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/common"
	pb "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/health-monitors/syslog-health-monitor/pkg/protos"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
)

const (
	maxJournalEntries    = 10000 // Maximum number of journal entries to process
	maxConsecutiveErrors = 5     // Maximum number of consecutive errors before giving up
	stateFileVersion     = 1     // Version of the state file format
)

// Field constants for journal entries
const (
	// Boot ID field in journal entries (underscore-prefixed in real journal).
	FieldBootID         = "_BOOT_ID"
	FieldMessage        = "MESSAGE"
	FieldSyslogFacility = "SYSLOG_FACILITY"
	FieldSystemdUnit    = "_SYSTEMD_UNIT"
)

// syslogMonitorState represents the persistent state of the syslog monitor
type syslogMonitorState struct {
	Version           int               `json:"version"`
	BootID           string            `json:"boot_id"`
	CheckLastCursors map[string]string `json:"check_last_cursors"`
}

// saveState saves the monitor state to a file
func saveState(stateFilePath string, state syslogMonitorState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(stateFilePath), 0755); err != nil {
		log.Fatalf("failed to create state directory: %v", err)
	}

	if err := os.WriteFile(stateFilePath, data, 0600); err != nil {
		log.Fatalf("failed to write state to file: %v", err)
	}

	return nil
}

// loadState loads the monitor state from a file
func loadState(stateFilePath string) (syslogMonitorState, error) {
	var state syslogMonitorState

	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			// Return default state for first run
			return syslogMonitorState{
				Version:           stateFileVersion,
				BootID:           "",
				CheckLastCursors: make(map[string]string),
			}, nil
		}
		return state, fmt.Errorf("failed to read state from file: %w", err)
	}

	// Check if file is empty
	if len(data) == 0 {
		klog.Warningf("State file %s exists but is empty, treating as non-existent", stateFilePath)
		return syslogMonitorState{
			Version:           stateFileVersion,
			BootID:           "",
			CheckLastCursors: make(map[string]string),
		}, nil
	}

	if err := json.Unmarshal(data, &state); err != nil {
		klog.Warningf("State file %s is corrupted: %v, resetting to default state", stateFilePath, err)
		return syslogMonitorState{
			Version:           stateFileVersion,
			BootID:           "",
			CheckLastCursors: make(map[string]string),
		}, nil
	}

	if state.Version != 0 && state.Version != stateFileVersion {
		if verifyStateFields(state) {
			klog.Infof("state file version mismatch: expected %d, got %d, but the old state file version is compatible",
				stateFileVersion, state.Version)
			// update the state version to latest current version
			state.Version = stateFileVersion

			if err := saveState(stateFilePath, state); err != nil {
				return state, fmt.Errorf("failed to save updated state: %w", err)
			}

			return state, nil
		}

		return state, fmt.Errorf("state file version mismatch: expected %d, got %d", stateFileVersion, state.Version)
	}

	// Ensure CheckLastCursors is not nil
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

// SyslogMonitor monitors journal logs for error patterns
type SyslogMonitor struct {
	nodeName              string
	checks                []common.CheckDefinition
	pcClient              pb.PlatformConnectorClient
	defaultAgentName      string
	defaultComponentClass string
	pollingInterval       string
	checkLastCursors      map[string]string // Map of check name to last processed cursor
	journalFactory        JournalFactory    // Factory for creating Journal instances
	stateFilePath         string            // Path to state file for persistence
	currentBootID         string            // Current system boot ID
}

// NewSyslogMonitor creates a new SyslogMonitor instance
func NewSyslogMonitor(nodeName string, checks []common.CheckDefinition, pcClient pb.PlatformConnectorClient, defaultAgentName string, defaultComponentClass string, pollingInterval string, stateFilePath string) (*SyslogMonitor, error) {
	return NewSyslogMonitorWithFactory(nodeName, checks, pcClient, defaultAgentName, defaultComponentClass, pollingInterval, stateFilePath, GetDefaultJournalFactory())
}

// NewSyslogMonitorWithFactory creates a new SyslogMonitor instance with a specific journal factory
func NewSyslogMonitorWithFactory(nodeName string, checks []common.CheckDefinition, pcClient pb.PlatformConnectorClient, defaultAgentName string, defaultComponentClass string, pollingInterval string, stateFilePath string, journalFactory JournalFactory) (*SyslogMonitor, error) {
	
	// Load state from file
	state, err := loadState(stateFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	// Get current boot ID
	currentBootID, err := fetchCurrentBootID()
	if err != nil {
		klog.Warningf("Failed to get current boot ID: %v", err)
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
		stateFilePath:         stateFilePath,
		currentBootID:         currentBootID,
	}

	// Handle boot ID changes (system reboot detection)
	if err := sm.handleBootIDChange(state.BootID, currentBootID); err != nil {
		return nil, fmt.Errorf("failed to handle boot ID change: %w", err)
	}

	klog.Info("SyslogMonitor initialized with persistent state. Each check will resume from last processed cursor.")
	return sm, nil
}

// handleBootIDChange handles system reboot detection and cursor reset
func (sm *SyslogMonitor) handleBootIDChange(oldBootID, newBootID string) error {
	if oldBootID != newBootID && oldBootID != "" {
		klog.Infof("Detected bootID change. Old bootID: %s, New bootID: %s", oldBootID, newBootID)
		
		// Clear all cursors on reboot since journal cursors become invalid
		for checkName := range sm.checkLastCursors {
			delete(sm.checkLastCursors, checkName)
		}
		
		// Save updated state
		state := syslogMonitorState{
			Version:           stateFileVersion,
			BootID:           newBootID,
			CheckLastCursors: sm.checkLastCursors,
		}
		
		if err := saveState(sm.stateFilePath, state); err != nil {
			return fmt.Errorf("failed to save state after boot ID change: %w", err)
		}
		
		klog.Info("Cleared all cursors due to system reboot")
	} else if oldBootID == "" {
		// First run, save current boot ID
		state := syslogMonitorState{
			Version:           stateFileVersion,
			BootID:           newBootID,
			CheckLastCursors: sm.checkLastCursors,
		}
		
		if err := saveState(sm.stateFilePath, state); err != nil {
			return fmt.Errorf("failed to save initial state: %w", err)
		}
		
		klog.Infof("Initialized state with boot ID: %s", newBootID)
	}
	
	return nil
}

// saveCurrentState saves the current state to the state file
func (sm *SyslogMonitor) saveCurrentState() error {
	state := syslogMonitorState{
		Version:           stateFileVersion,
		BootID:           sm.currentBootID,
		CheckLastCursors: sm.checkLastCursors,
	}
	
	return saveState(sm.stateFilePath, state)
}

// executeCheck performs a single log check based on the provided definition
func (sm *SyslogMonitor) executeCheck(check common.CheckDefinition) error {
	klog.Infof("--- Executing Check: %s ---", check.Name)

	if err := sm.validateCheckDefinition(check); err != nil {
		return err
	}

	journal, err := sm.openJournal(check)
	if err != nil {
		return err
	}
	defer journal.Close()

	if err := sm.configureTagFilters(journal, check); err != nil {
		return err
	}

	patterns, err := sm.compilePatterns(check)
	if err != nil {
		return err
	}

	matchingLines, err := sm.processJournalEntries(journal, patterns, check)
	if err != nil {
		return err
	}

	// Save state after successfully processing journal entries
	if err := sm.saveCurrentState(); err != nil {
		klog.Warningf("Failed to save state after processing check '%s': %v", check.Name, err)
	}

	return sm.evaluateResults(check, matchingLines)
}

// validateCheckDefinition validates the check configuration
func (sm *SyslogMonitor) validateCheckDefinition(check common.CheckDefinition) error {
	if len(check.Matches) == 0 {
		return fmt.Errorf("check '%s': 'matches' must not be empty", check.Name)
	}
	if check.Count < 0 {
		return fmt.Errorf("check '%s': 'count' must be non-negative", check.Name)
	}
	return nil
}

// openJournal opens the systemd journal with the specified path
func (sm *SyslogMonitor) openJournal(check common.CheckDefinition) (Journal, error) {
	if check.JournalPath != "" {
		klog.Infof("Check '%s': Verifying journal path: %s", check.Name, check.JournalPath)

		// Only validate path on filesystem for real journal factories
		if sm.journalFactory.RequiresFileSystemCheck() {
			fileInfo, err := os.Stat(check.JournalPath)
			if err != nil {
				if os.IsNotExist(err) {
					return nil, fmt.Errorf("check '%s': journal path does not exist: %s", check.Name, check.JournalPath)
				}
				return nil, fmt.Errorf("check '%s': error accessing journal path %s: %v", check.Name, check.JournalPath, err)
			}
			if !fileInfo.IsDir() {
				return nil, fmt.Errorf("check '%s': journal path is not a directory: %s", check.Name, check.JournalPath)
			}
		}

		klog.Infof("Check '%s': Opening journal at path: %s", check.Name, check.JournalPath)
		journal, err := sm.journalFactory.NewJournalFromDir(check.JournalPath)
		if err != nil {
			return nil, fmt.Errorf("check '%s': failed to open journal from dir %s: %v", check.Name, check.JournalPath, err)
		}
		return journal, nil
	} else {
		return nil, fmt.Errorf("check '%s': journal path is empty. Path-specific journal expected for checks.", check.Name)
	}
}

// configureBootFilter sets up the boot filter for the journal
func (sm *SyslogMonitor) configureBootFilter(journal Journal, checkName string) error {
	bootID := sm.getCurrentBootID()
	if bootID != "" {
		matchExpr := FieldBootID + "=" + bootID
		klog.Infof("Check '%s': Applying boot filter: %s", checkName, matchExpr)
		if err := journal.AddMatch(matchExpr); err != nil {
			return fmt.Errorf("check '%s': failed to add boot ID match ('%s'): %v", checkName, matchExpr, err)
		}
	} else {
		klog.Warningf("Check '%s': Could not determine current boot ID. Boot filter will not be applied.", checkName)
	}
	return nil
}

// configureTagFilters sets up the tag-based filters for the journal
func (sm *SyslogMonitor) configureTagFilters(journal Journal, check common.CheckDefinition) error {
	for _, tag := range check.Tags {
		trimmedTag := strings.TrimSpace(tag)
		if trimmedTag == "" {
			continue
		}

		switch trimmedTag {
		case "-k", "--dmesg":
			// Facility 0 is typically KERNEL messages.
			matchExpr := FieldSyslogFacility + "=0"
			klog.Infof("Check '%s': Adding kernel log filter from tag '%s': %s", check.Name, trimmedTag, matchExpr)
			if err := journal.AddMatch(matchExpr); err != nil {
				return fmt.Errorf("check '%s': failed to add kernel match ('%s'): %v", check.Name, matchExpr, err)
			}
		case "-b", "--boot":
			klog.Infof("Check '%s': Processing explicit boot tag '%s'. Boot filter is primarily configured via check.Boot flag.", check.Name, trimmedTag)
			// configureBootFilter is already called if check.Boot is true.
			// Calling it again here due to an explicit tag is generally harmless if configureBootFilter is idempotent.
			if err := sm.configureBootFilter(journal, check.Name); err != nil {
				return err // Error message from configureBootFilter should be sufficient
			}
		default:
			if strings.HasPrefix(trimmedTag, "-u ") || strings.HasPrefix(trimmedTag, "--unit ") {
				var unitName string
				if strings.HasPrefix(trimmedTag, "-u ") {
					unitName = strings.TrimSpace(strings.TrimPrefix(trimmedTag, "-u "))
				} else { // Must be --unit due to the check above
					unitName = strings.TrimSpace(strings.TrimPrefix(trimmedTag, "--unit "))
				}

				if unitName != "" {
					matchExpr := FieldSystemdUnit + "=" + unitName
					klog.Infof("Check '%s': Adding unit filter from tag '%s': %s", check.Name, trimmedTag, matchExpr)
					if err := journal.AddMatch(matchExpr); err != nil {
						return fmt.Errorf("check '%s': failed to add unit match for '%s' (using expression '%s'): %v", check.Name, unitName, matchExpr, err)
					}
				} else {
					klog.Warningf("Check '%s': Tag '%s' for unit filtering resulted in an empty unit name after parsing.", check.Name, trimmedTag)
				}
			} else {
				klog.Infof("Check '%s': Ignoring unrecognized tag in 'configureTagFilters': '%s'", check.Name, trimmedTag)
			}
		}
	}
	return nil
}

// compilePatterns compiles the regex patterns for matching
func (sm *SyslogMonitor) compilePatterns(check common.CheckDefinition) ([]*regexp.Regexp, error) {
	var patterns []*regexp.Regexp
	for _, match := range check.Matches {
		var pattern *regexp.Regexp
		var err error
		if check.IgnoreCase {
			pattern, err = regexp.Compile("(?i)" + match)
		} else {
			pattern, err = regexp.Compile(match)
		}
		if err != nil {
			return nil, fmt.Errorf("check '%s': invalid regex pattern '%s': %v", check.Name, match, err)
		}
		patterns = append(patterns, pattern)
	}
	return patterns, nil
}

// processJournalEntries reads and processes journal entries
func (sm *SyslogMonitor) processJournalEntries(journal Journal, patterns []*regexp.Regexp, check common.CheckDefinition) ([]string, error) {
	var matchingLines []string
	entryCount := 0
	// currentEntryCursor will store the cursor of the entry currently being processed or just processed.
	// sm.checkLastCursors[checkName] will store the cursor to resume from on the NEXT run.

	lastKnownCursor, hasLastCursor := sm.checkLastCursors[check.Name]
	bootID, err := journal.GetBootID()
	if err != nil {
		klog.Warningf("Check '%s': Failed to get boot ID: %v", check.Name, err)
	}
	klog.Infof("Check '%s': Boot ID: %s", check.Name, bootID)
	// This block handles:
	// 1. Non-boot checks on their first run (hasLastCursor == false)
	// 2. All checks (boot or non-boot) on subsequent runs (hasLastCursor == true)
	if !hasLastCursor { // This implies !check.Boot due to the block above
		klog.Infof("Check '%s': No last known cursor. Seeking to journal tail to establish a starting point for future entries.", check.Name)
		if err := journal.SeekTail(); err != nil {
			return nil, fmt.Errorf("check '%s': failed to seek to journal tail for initialization: %v", check.Name, err)
		}
		count, errPrev := journal.Previous()
		if errPrev != nil && errPrev != io.EOF {
			return nil, fmt.Errorf("seek previous: %w", errPrev)
		}
		if count == 0 {                 // journal is empty
			klog.Infof("Check %q: journal empty, nothing to do", check.Name)
			return nil, nil
		}
		cursor, err := journal.GetCursor()
		if err != nil {
			if strings.Contains(err.Error(), "cannot assign requested address") {
				klog.Infof("Check %q: no cursor (journal empty); will try again next run", check.Name)
				return nil, nil
			}
			return nil, fmt.Errorf("get cursor: %w", err)
		}
		klog.Infof("Check '%s': Initialized. Journal processing will start from entries after cursor '%s' on the next run.", check.Name, cursor)
		sm.checkLastCursors[check.Name] = cursor
		return matchingLines, nil // No entries processed on this initialization run.
	}

	// If we are here, hasLastCursor is true.
	klog.Infof("Check '%s': Resuming from last known cursor: %s", check.Name, lastKnownCursor)
	if err := journal.SeekCursor(lastKnownCursor); err != nil {
		klog.Warningf("Check '%s': Failed to seek to last known cursor '%s': %v. Re-initializing by seeking to current tail.", check.Name, lastKnownCursor, err)
		if errSeekTail := journal.SeekTail(); errSeekTail != nil {
			return nil, fmt.Errorf("check '%s': failed to seek to journal tail during re-initialization after SeekCursor error: %v", check.Name, errSeekTail)
		}
		tailCursor, errGetCursor := journal.GetCursor()
		if errGetCursor != nil {
			return nil, fmt.Errorf("check '%s': failed to get cursor at journal tail during re-initialization: %v", check.Name, errGetCursor)
		}
		klog.Infof("Check '%s': Re-initialized. Journal processing will start from entries after cursor '%s' on the next run.", check.Name, tailCursor)
		sm.checkLastCursors[check.Name] = tailCursor
		return matchingLines, nil // No entries processed on this re-initialization run.
	}

	// Successfully sought to lastKnownCursor. Now advance to the *next* entry.
	// This is crucial: we process entries *after* the lastKnownCursor.
	advanced, nextErr := journal.Next()
	if nextErr != nil && nextErr != io.EOF {
		return nil, fmt.Errorf("check '%s': error advancing from resumed cursor '%s': %v", check.Name, lastKnownCursor, nextErr)
	}
	if nextErr == io.EOF || advanced == 0 {
		klog.Infof("Check '%s': No new entries since last cursor %s.", check.Name, lastKnownCursor)
		// sm.checkLastCursors[checkName] is already lastKnownCursor, which is correct for the next run.
		return matchingLines, nil
	}

	// Journal cursor is now positioned at the first new entry to process.
	for {
		entryCount++
		if entryCount > maxJournalEntries {
			klog.Warningf("Check '%s': Reached maximum entry processing limit of %d for this cycle. Will resume after cursor %s on next run.", check.Name, maxJournalEntries, sm.checkLastCursors[check.Name])
			break // sm.checkLastCursors[checkName] holds the cursor of the last processed entry.
		}

		currentEntryCursor, err := journal.GetCursor() // Cursor of the entry we are about to process
		if err != nil {
			klog.Warningf("Check '%s': Failed to get cursor for current entry: %v. Attempting to advance. Last stored cursor for next run: %s.", check.Name, err, sm.checkLastCursors[check.Name])
			advancedNext, advErr := journal.Next()
			if advErr == io.EOF || advancedNext == 0 {
				klog.Infof("Check '%s': Reached end of journal while trying to recover from GetCursor error. Next run will start after: %s.", check.Name, sm.checkLastCursors[check.Name])
				break
			} else if advErr != nil {
				klog.Errorf("Check '%s': Error advancing journal after GetCursor error: %v. Stopping processing. Next run will start after: %s.", check.Name, advErr, sm.checkLastCursors[check.Name])
				return matchingLines, fmt.Errorf("error advancing after GetCursor error for check '%s' (last stored cursor for next run %s): %v", check.Name, sm.checkLastCursors[check.Name], advErr)
			}
			continue // Skip to the next iteration
		}

		message, err := sm.getJournalMessage(journal, check.Name)
		if err != nil {
			klog.Warningf("Check '%s': Failed to get journal message for entry at cursor %s: %v. Skipping. Next run will start after: %s.", check.Name, currentEntryCursor, err, sm.checkLastCursors[check.Name])
			advancedNext, advErr := journal.Next()
			if advErr == io.EOF || advancedNext == 0 {
				klog.Infof("Check '%s': Reached end of journal while trying to recover from getJournalMessage error for cursor %s. Next run will start after: %s.", check.Name, currentEntryCursor, sm.checkLastCursors[check.Name])
				break
			} else if advErr != nil {
				klog.Errorf("Check '%s': Error advancing journal after getJournalMessage error for cursor %s: %v. Stopping. Next run will start after: %s.", check.Name, currentEntryCursor, advErr, sm.checkLastCursors[check.Name])
				return matchingLines, fmt.Errorf("error advancing after getJournalMessage for check '%s' (entry cursor %s, last stored cursor for next run %s): %v", check.Name, currentEntryCursor, sm.checkLastCursors[check.Name], advErr)
			}
			continue
		}

		if message == "" {
			// Successfully read an empty message. This entry is considered processed.
			sm.checkLastCursors[check.Name] = currentEntryCursor // Update cursor for the next run
			klog.Infof("Check '%s': Empty message at cursor %s. Stored cursor for next run. Advancing.", check.Name, currentEntryCursor)
		} else {
			if sm.messageMatchesPatterns(message, patterns) {
				matchingLines = append(matchingLines, message)
			}
			// This entry (matched or not) is considered processed.
			sm.checkLastCursors[check.Name] = currentEntryCursor // Update cursor for the next run
			klog.Infof("Check '%s': Processed entry at cursor %s. Stored cursor for next run. Advancing.", check.Name, currentEntryCursor)
		}

		advancedNext, advErr := journal.Next()
		if advErr == io.EOF || advancedNext == 0 {
			klog.Infof("Check '%s': Reached end of journal after processing entry with cursor %s. Next run will start after this cursor.", check.Name, currentEntryCursor)
			// sm.checkLastCursors[checkName] is already set to currentEntryCursor.
			break
		}
		if advErr != nil {
			// Error advancing. currentEntryCursor was the last successfully processed one.
			klog.Errorf("Check '%s': Error reading next journal entry after cursor %s: %v. Next run will start after this cursor.", check.Name, currentEntryCursor, advErr)
			return matchingLines, fmt.Errorf("check '%s': error reading next journal entry after cursor %s: %v", check.Name, currentEntryCursor, advErr)
		}
	}

	finalCursor := sm.checkLastCursors[check.Name] // Should always exist if we passed initialization.
	klog.Infof("Check '%s': Finished processing journal entries for this cycle. Found %d matches. Next run will start after cursor: %s", check.Name, len(matchingLines), finalCursor)
	return matchingLines, nil
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
			return "", err
		}

		// Log retry attempt
		if i < maxRetries-1 {
			klog.V(4).Infof("Check '%s': Retrying journal message read (attempt %d/%d): %v",
				checkName, i+1, maxRetries, err)
			time.Sleep(retryDelay)
		}
	}

	return "", fmt.Errorf("failed to read journal message after %d attempts: %v", maxRetries, err)
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

// messageMatchesPatterns checks if a message matches any of the patterns
func (sm *SyslogMonitor) messageMatchesPatterns(message string, patterns []*regexp.Regexp) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(message) {
			return true
		}
	}
	return false
}

// evaluateResults evaluates the check results and sends health events if needed
func (sm *SyslogMonitor) evaluateResults(check common.CheckDefinition, matchingLines []string) error {
	numMatches := len(matchingLines)
	klog.Infof("Check '%s': Found %d matching lines (threshold: %d).", check.Name, numMatches, check.Count)

	if numMatches > check.Count {
		klog.Errorf("Error for check '%s': Found %d lines, which is more than the allowed count of %d.", check.Name, numMatches, check.Count)
		klog.Errorln("Matching lines:")
		for _, line := range matchingLines {
			klog.Errorln(line)
		}

		errMsg := fmt.Sprintf("Found %d matches, threshold is %d. Lines: %s", numMatches, check.Count, strings.Join(matchingLines, "\\n"))
		healthEvents := sm.prepareHealthEvent(check, errMsg, false, true)

		if sm.pcClient != nil {
			sm.sendHealthEventWithRetry(healthEvents, 5, 2*time.Second)
		} else {
			klog.Warningf("Platform connector client is nil, cannot send health event for check '%s'", check.Name)
		}

		klog.Infof("check '%s' FAILED: %s", check.Name, errMsg)
		return nil
	}

	klog.Infof("Check '%s' PASSED.", check.Name)
	return nil
}

// getCurrentBootID returns the current system boot ID
func (sm *SyslogMonitor) getCurrentBootID() string {
	journal, err := sm.journalFactory.NewJournal()
	if err != nil {
		klog.Warningf("Failed to open system journal for boot ID: %v", err)
		return ""
	}
	defer journal.Close()

	bootID, err := journal.GetBootID()
	if err != nil {
		klog.Warningf("Failed to get boot ID: %v", err)
		return ""
	}
	return bootID
}

// prepareHealthEvent creates a health event for reporting
func (sm *SyslogMonitor) prepareHealthEvent(check common.CheckDefinition, message string, isHealthy bool, isFatal bool) *pb.HealthEvents {
	klog.Infof("Preparing health event for check '%s': Message: %s, Healthy: %t, Fatal: %t", check.Name, message, isHealthy, isFatal)

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              sm.defaultAgentName,
		CheckName:          check.Name,
		ComponentClass:     sm.defaultComponentClass,
		GeneratedTimestamp: timestamppb.New(time.Now()),
		EntitiesImpacted:   []*pb.Entity{{EntityType: "Node", EntityValue: sm.nodeName}},
		Message:            message,
		IsFatal:            isFatal,
		IsHealthy:          isHealthy,
		NodeName:           sm.nodeName,
		RecommendedAction:  pb.RecommenedAction_REPORT_ISSUE,
	}
	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}

// sendHealthEventWithRetry sends health events to platform connector with retry logic
func (sm *SyslogMonitor) sendHealthEventWithRetry(healthEvents *pb.HealthEvents, maxRetries int, retryDelay time.Duration) {
	if sm.pcClient == nil {
		klog.Error("PlatformConnectorClient is nil, cannot send health event.")
		return
	}
	klog.Infof("Attempting to send health event: %+v", healthEvents)

	backoff := wait.Backoff{
		Steps:    maxRetries,
		Duration: retryDelay,
		Factor:   1.5,
		Jitter:   0.1,
	}

	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		_, err := sm.pcClient.HealthEventOccuredV1(context.Background(), healthEvents)
		if err == nil {
			klog.Infof("Successfully sent health events: %+v", healthEvents)
			return true, nil
		}

		if isRetryableError(err) {
			klog.Warningf("Retryable error occurred while sending health event: %v. Retrying...", err)
			return false, nil
		}

		klog.Errorf("Non-retryable error occurred while sending health event: %v", err)
		return false, err
	})

	if err != nil {
		klog.Errorf("All retry attempts to send health event failed: %v", err)
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

	if err == io.EOF || strings.Contains(err.Error(), "connection reset by peer") || strings.Contains(err.Error(), "broken pipe") {
		return true
	}

	return false
}

// Run executes all configured checks
func (sm *SyslogMonitor) Run() error {
	var firstError error = nil

	klog.Infof("Starting syslog monitor run cycle.")

	for _, check := range sm.checks {
		err := sm.executeCheck(check)
		if err != nil {
			klog.Errorf("Check '%s' failed during execution: %v", check.Name, err)
			if firstError == nil {
				firstError = err
			}
		}
	}

	if firstError != nil {
		klog.Infof("Syslog monitor run cycle completed with one or more errors.")
		return firstError
	}

	klog.Infof("Syslog monitor run cycle completed successfully.")
	return nil
}
