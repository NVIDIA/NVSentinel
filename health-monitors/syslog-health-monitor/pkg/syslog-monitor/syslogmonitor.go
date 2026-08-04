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

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/cancellation"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/gpufallen"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/nicdriver"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/sxid"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/xid"
)

// NewSyslogMonitor creates a new SyslogMonitor instance. cancellationsCfg may be nil.
//
// platformConnectorTarget is the gRPC target used to dial pcClient
// (e.g. "unix:///var/run/nvsentinel.sock"). Pass it through here so the
// shared healthpub publisher's socket-existence gate is active from the
// first send (in particular, the post-reboot healthy events emitted by
// handleBootIDChange during construction). An empty string disables
// the gate.
func NewSyslogMonitor(
	nodeName string,
	checks []CheckDefinition,
	pcClient pb.PlatformConnectorClient,
	defaultAgentName string,
	defaultComponentClass string,
	pollingInterval string,
	stateFilePath string,
	xidAnalyserEndpoint string,
	metadataPath string,
	processingStrategy pb.ProcessingStrategy,
	nicDriverConfigPath string,
	sysfsRoot string,
	cancellationsCfg *cancellation.Config,
	platformConnectorTarget string,
) (*SyslogMonitor, error) {
	return NewSyslogMonitorWithFactory(nodeName, checks, pcClient, defaultAgentName,
		defaultComponentClass, pollingInterval, stateFilePath, GetDefaultJournalFactory(),
		xidAnalyserEndpoint, metadataPath,
		processingStrategy,
		nicDriverConfigPath, sysfsRoot,
		cancellationsCfg,
		platformConnectorTarget,
	)
}

// NewSyslogMonitorWithFactory creates a new SyslogMonitor instance with
// a specific journal factory. See NewSyslogMonitor for the meaning of
// platformConnectorTarget.
func NewSyslogMonitorWithFactory(
	nodeName string,
	checks []CheckDefinition,
	pcClient pb.PlatformConnectorClient,
	defaultAgentName string,
	defaultComponentClass string,
	pollingInterval string,
	stateFilePath string,
	journalFactory JournalFactory,
	xidAnalyserEndpoint string,
	metadataPath string,
	processingStrategy pb.ProcessingStrategy,
	nicDriverConfigPath string,
	sysfsRoot string,
	cancellationsCfg *cancellation.Config,
	platformConnectorTarget string,
) (*SyslogMonitor, error) {
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
		nodeName:                nodeName,
		checks:                  checks,
		pcClient:                pcClient,
		defaultAgentName:        defaultAgentName,
		defaultComponentClass:   defaultComponentClass,
		processingStrategy:      processingStrategy,
		pollingInterval:         pollingInterval,
		checkLastCursors:        state.CheckLastCursors,
		journalFactory:          journalFactory,
		currentBootID:           currentBootID,
		stateFilePath:           stateFilePath,
		checkToHandlerMap:       make(map[string]types.Handler),
		xidAnalyserEndpoint:     xidAnalyserEndpoint,
		platformConnectorTarget: platformConnectorTarget,
	}

	if err := initHandlers(sm, checks, nodeName, defaultAgentName, defaultComponentClass,
		xidAnalyserEndpoint, metadataPath, processingStrategy,
		nicDriverConfigPath, sysfsRoot, cancellationsCfg); err != nil {
		return nil, err
	}

	// Handle boot ID changes (system reboot detection)
	if err := sm.handleBootIDChange(state.BootID, currentBootID); err != nil {
		return nil, fmt.Errorf("failed to handle boot ID change: %w", err)
	}

	slog.Info("SyslogMonitor initialized with persistent state. Each check will resume from last processed cursor.")

	return sm, nil
}

// initHandlers creates and registers a handler for each check. Unsupported check names are logged and skipped.
func initHandlers(
	sm *SyslogMonitor,
	checks []CheckDefinition,
	nodeName string,
	defaultAgentName string,
	defaultComponentClass string,
	xidAnalyserEndpoint string,
	metadataPath string,
	processingStrategy pb.ProcessingStrategy,
	nicDriverConfigPath string,
	sysfsRoot string,
	cancellationsCfg *cancellation.Config,
) error {
	for _, check := range checks {
		handler, err := initHandlerForCheck(check, nodeName, defaultAgentName, defaultComponentClass,
			xidAnalyserEndpoint, metadataPath, processingStrategy,
			nicDriverConfigPath, sysfsRoot, cancellationsCfg)
		if err != nil {
			return err
		}

		if handler == nil {
			slog.Error("Unsupported check", "check", check.Name)
			continue
		}

		sm.checkToHandlerMap[check.Name] = handler
	}

	return nil
}

// initHandlerForCheck creates a Handler for the given check. Returns (nil, nil) for unsupported check names.
func initHandlerForCheck(
	check CheckDefinition,
	nodeName string,
	defaultAgentName string,
	defaultComponentClass string,
	xidAnalyserEndpoint string,
	metadataPath string,
	processingStrategy pb.ProcessingStrategy,
	nicDriverConfigPath string,
	sysfsRoot string,
	cancellationsCfg *cancellation.Config,
) (types.Handler, error) {
	switch check.Name {
	case XIDErrorCheck:
		h, err := xid.NewXIDHandler(nodeName, defaultAgentName, defaultComponentClass, check.Name,
			xidAnalyserEndpoint, metadataPath, processingStrategy)
		if err != nil {
			slog.Error("Error initializing XID handler", "error", err.Error())
			return nil, fmt.Errorf("failed to initialize XID handler: %w", err)
		}

		h.SetCancellationResolver(cancellation.NewResolver(cancellationsCfg.FindCheck(check.Name)))

		return h, nil
	case SXIDErrorCheck:
		h, err := sxid.NewSXIDHandler(nodeName, defaultAgentName, defaultComponentClass, check.Name,
			metadataPath, processingStrategy)
		if err != nil {
			slog.Error("Error initializing SXID handler", "error", err.Error())
			return nil, fmt.Errorf("failed to initialize SXID handler: %w", err)
		}

		return h, nil
	case GPUFallenOffCheck:
		h, err := gpufallen.NewGPUFallenHandler(nodeName, defaultAgentName, defaultComponentClass, check.Name,
			processingStrategy)
		if err != nil {
			slog.Error("Error initializing GPU Fallen Off handler", "error", err.Error())
			return nil, fmt.Errorf("failed to initialize GPU Fallen Off handler: %w", err)
		}

		return h, nil
	case NICDriverErrorCheck:
		h, err := nicdriver.NewNICDriverHandler(nodeName, defaultAgentName, check.Name,
			nicDriverConfigPath, sysfsRoot, processingStrategy)
		if err != nil {
			slog.Error("Error initializing NIC Driver handler", "error", err.Error())
			return nil, fmt.Errorf("failed to initialize NIC Driver handler: %w", err)
		}

		return h, nil
	default:
		return nil, nil
	}
}

// Run executes all configured checks. If a previous bootID-change had
// to defer its healthy events because platform-connector was missing,
// retry that flush first so recovery is bounded by one polling cadence
// after PC returns rather than by process lifetime.
//
// When the flush is still deferred at the end of this attempt the rest
// of the cycle is skipped: executeCheck calls saveCurrentState which
// would otherwise persist sm.currentBootID (the new BootID) and clobber
// the on-disk old BootID, silently breaking the "retry until delivered"
// guarantee. The next Run() retries the flush.
func (sm *SyslogMonitor) Run() error {
	var jointError error = nil

	if err := sm.tryFlushPostRebootBootIDClear(); err != nil {
		slog.Error("Pending post-reboot bootID flush failed",
			"error", err)

		jointError = errors.Join(jointError, err)
	}

	if sm.pendingPostRebootBootID != "" {
		slog.Warn("Skipping check execution: post-reboot bootID flush still pending. " +
			"Will retry on next cycle; not running checks to avoid persisting the new " +
			"BootID before the post-reboot healthy events have been delivered.")

		return jointError
	}

	for _, check := range sm.checks {
		err := sm.executeCheck(check)
		if err != nil {
			slog.Error("Check failed during execution",
				"check", check.Name,
				"error", err)

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

// loadState loads the monitor state from a file.
func loadState(stateFilePath string) (syslogMonitorState, error) {
	data, err := readStateFile(stateFilePath)
	if err != nil {
		return syslogMonitorState{}, err
	}

	if data == nil {
		return newDefaultState(), nil
	}

	state, ok := parseStateData(stateFilePath, data)
	if !ok {
		return newDefaultState(), nil
	}

	// Version migration needed if not zero and not current
	if state.Version != 0 && state.Version != stateFileVersion {
		return migrateStateVersion(stateFilePath, state)
	}

	return ensureStateInitialized(state), nil
}

// newDefaultState returns a fresh state for first run or after reset.
func newDefaultState() syslogMonitorState {
	return syslogMonitorState{
		Version:          stateFileVersion,
		BootID:           "",
		CheckLastCursors: make(map[string]string),
	}
}

// readStateFile reads the state file. Returns (nil, nil) if file does not exist; (nil, err) on read error.
func readStateFile(stateFilePath string) ([]byte, error) {
	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("failed to read state from file: %w", err)
	}

	return data, nil
}

// parseStateData unmarshals state from data. Returns (state, true) on success; (zero, false) if empty or corrupt.
func parseStateData(stateFilePath string, data []byte) (syslogMonitorState, bool) {
	if len(data) == 0 {
		slog.Warn("State file exists but is empty, treating as non-existent",
			"stateFile", stateFilePath)

		return syslogMonitorState{}, false
	}

	var state syslogMonitorState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("State file is corrupted, resetting to default",
			"stateFile", stateFilePath,
			"error", err)

		return syslogMonitorState{}, false
	}

	return state, true
}

// verifyStateFields verifies if necessary fields for current state version are present
func verifyStateFields(state syslogMonitorState) bool {
	// For syslog monitor, we mainly need the CheckLastCursors map to exist
	return state.CheckLastCursors != nil
}

// migrateStateVersion updates state to current version if compatible, or returns an error.
func migrateStateVersion(stateFilePath string, state syslogMonitorState) (syslogMonitorState, error) {
	if verifyStateFields(state) {
		slog.Info("State file version mismatch but compatible",
			"expected", stateFileVersion,
			"actual", state.Version)

		state.Version = stateFileVersion
		if err := saveState(stateFilePath, state); err != nil {
			return state, fmt.Errorf("failed to save updated state: %w", err)
		}

		return state, nil
	}

	return state, fmt.Errorf("state file version mismatch: expected %d, got %d", stateFileVersion, state.Version)
}

// ensureStateInitialized ensures required maps are non-nil.
func ensureStateInitialized(state syslogMonitorState) syslogMonitorState {
	if state.CheckLastCursors == nil {
		state.CheckLastCursors = make(map[string]string)
	}

	return state
}

// fetchCurrentBootID returns the current system boot ID
func fetchCurrentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("failed to read boot_id: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// handleBootIDChange handles system reboot detection and cursor reset.
//
// On reboot the journal cursors persisted on disk no longer point at
// valid offsets, so we clear them and emit one healthy event per check
// to clear any stuck quarantine state in fault-quarantine.
//
// The new BootID is persisted to disk only after every healthy event
// has been delivered. If any send is skipped (platform-connector
// socket missing), sm.pendingPostRebootBootID is left set so Run()
// retries the flush at the top of each poll cycle, bounding recovery
// to one polling cadence after PC returns.
func (sm *SyslogMonitor) handleBootIDChange(oldBootID, newBootID string) error {
	if oldBootID == newBootID {
		return nil
	}

	slog.Info("Detected bootID change",
		"oldBootID", oldBootID,
		"newBootID", newBootID)

	// Clear cursors in memory so the rest of this process polls the
	// journal from its current position. Persistence is deferred to
	// tryFlushPostRebootBootIDClear, conditional on all sends landing.
	for checkName := range sm.checkLastCursors {
		delete(sm.checkLastCursors, checkName)
	}

	sm.pendingPostRebootBootID = newBootID

	return sm.tryFlushPostRebootBootIDClear()
}

// tryFlushPostRebootBootIDClear emits one healthy event per check for
// the pending bootID change and persists the new BootID + cleared
// cursors only when all events land. On a healthpub-skip
// (ErrPlatformConnectorUnavailable) the pending flag is left set so
// the next call retries; on any other send error the function returns
// fatal — that surface bubbles up to Run() which the main ticker loop
// already retries with backoff.
//
// Idempotent and safe to call repeatedly. A no-op when there is no
// pending bootID change.
func (sm *SyslogMonitor) tryFlushPostRebootBootIDClear() error {
	if sm.pendingPostRebootBootID == "" {
		return nil
	}

	allDelivered := true

	for _, check := range sm.checks {
		message := "No Health Failures"
		errRes := types.ErrorResolution{
			RecommendedAction: pb.RecommendedAction_NONE,
		}

		healthEvents := sm.prepareHealthEventWithAction(check, message, true, errRes)
		if err := sm.sendHealthEventWithRetry(healthEvents, 5, 2*time.Second); err != nil {
			if errors.Is(err, healthpub.ErrPlatformConnectorUnavailable) {
				slog.Warn("Deferring post-reboot healthy event: platform-connector unavailable.",
					"check", check.Name)

				allDelivered = false

				continue
			}

			return fmt.Errorf("failed to send health event: %w", err)
		}

		slog.Info("Published healthy event after system reboot", "check", check.Name)
	}

	if !allDelivered {
		slog.Warn("Post-reboot healthy events deferred; will retry on next poll cycle.")

		return nil
	}

	state := syslogMonitorState{
		Version:          stateFileVersion,
		BootID:           sm.pendingPostRebootBootID,
		CheckLastCursors: sm.checkLastCursors,
	}

	if err := saveState(sm.stateFilePath, state); err != nil {
		return fmt.Errorf("failed to save state after boot ID change: %w", err)
	}

	slog.Info("Cleared all cursors due to system reboot",
		"bootID", sm.pendingPostRebootBootID)

	sm.pendingPostRebootBootID = ""

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
			slog.Warn("Error closing journal",
				"check", check.Name,
				"error", cerr)
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
		slog.Warn("Failed to save state after processing check",
			"check", check.Name,
			"error", err)
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
	if check.JournalPath == "" {
		return nil, fmt.Errorf("check '%s': journal path is empty. Path-specific journal expected for checks", check.Name)
	}

	slog.Info("Verifying journal path",
		"check", check.Name,
		"path", check.JournalPath)

	if sm.journalFactory.RequiresFileSystemCheck() {
		if err := sm.validateJournalPath(check); err != nil {
			return nil, fmt.Errorf("journal path validation failed for check %s: %w", check.Name, err)
		}
	}

	slog.Info("Opening journal at path",
		"check", check.Name,
		"path", check.JournalPath)

	journal, err := sm.journalFactory.NewJournalFromDir(check.JournalPath)
	if err != nil {
		return nil, fmt.Errorf("check '%s': failed to open journal from dir %s: %w", check.Name, check.JournalPath, err)
	}

	return journal, nil
}

// configureBootFilter sets up the boot filter for the journal
func (sm *SyslogMonitor) configureBootFilter(journal Journal, checkName string) error {
	bootID := sm.getCurrentBootID()
	if bootID != "" {
		matchExpr := FieldBootID + "=" + bootID

		slog.Info("Applying boot filter",
			"check", checkName,
			"filter", matchExpr)

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
//nolint:gocognit,cyclop // single switch over tag types; splitting would reduce clarity
func (sm *SyslogMonitor) configureTagFilters(journal Journal, check CheckDefinition) error {
	for j := 0; j < len(check.Tags); j++ {
		trimmedTag := strings.TrimSpace(check.Tags[j])
		if trimmedTag == "" {
			continue
		}

		switch trimmedTag {
		case "-k", "--dmesg":
			// Kernel-transport logs (printk + /dev/kmsg), independent of the
			// syslog facility. This is generic across devices that may not set
			// SYSLOG_FACILITY=kern, and naturally excludes journal/audit noise.
			matchExpr := FieldTransport + "=" + TransportKernel

			slog.Info("Adding kernel log filter",
				"check", check.Name,
				"tag", trimmedTag,
				"match", matchExpr)

			if err := journal.AddMatch(matchExpr); err != nil {
				return fmt.Errorf("check '%s': failed to add kernel match ('%s'): %w", check.Name, matchExpr, err)
			}

			// GPU reset acknowledgements are emitted through logger rather than
			// the kernel. Keep them visible to the XID handler without admitting
			// unrelated userspace logs:
			// (_TRANSPORT=kernel) OR (SYSLOG_IDENTIFIER=nvsentinel-gpu-reset).
			if check.Name == XIDErrorCheck {
				if err := journal.AddDisjunction(); err != nil {
					return fmt.Errorf("check '%s': failed to add GPU reset filter disjunction: %w", check.Name, err)
				}

				resetMatchExpr := FieldSyslogID + "=" + GPUResetSyslogID
				slog.Info("Adding GPU reset acknowledgement filter",
					"check", check.Name,
					"match", resetMatchExpr)

				if err := journal.AddMatch(resetMatchExpr); err != nil {
					return fmt.Errorf("check '%s': failed to add GPU reset match ('%s'): %w",
						check.Name, resetMatchExpr, err)
				}
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
		case "-u", "--unit":
			// Standalone flag: next tag element is the unit name
			if j+1 >= len(check.Tags) {
				slog.Warn("Tag for unit filtering missing unit name (end of list)",
					"check", check.Name,
					"tag", trimmedTag)

				continue
			}

			j++

			unitName := strings.TrimSpace(check.Tags[j])
			if unitName == "" {
				slog.Warn("Tag for unit filtering resulted in empty unit name",
					"check", check.Name,
					"tag", trimmedTag)

				continue
			}

			matchExpr := FieldSystemdUnit + "=" + unitName
			slog.Info("Adding unit filter",
				"check", check.Name,
				"tag", trimmedTag,
				"match", matchExpr)

			if err := journal.AddMatch(matchExpr); err != nil {
				return fmt.Errorf("check '%s': failed to add unit match for '%s' (using expression '%s'): %w",
					check.Name, unitName, matchExpr, err)
			}
		default:
			if !strings.HasPrefix(trimmedTag, "-u ") && !strings.HasPrefix(trimmedTag, "--unit ") {
				slog.Info("Ignoring unrecognized tag in 'configureTagFilters'",
					"check", check.Name,
					"tag", trimmedTag)

				continue
			}

			// Combined flag: unit name in same element (e.g. "-u containerd.service")
			var unitName string
			if strings.HasPrefix(trimmedTag, "-u ") {
				unitName = strings.TrimSpace(strings.TrimPrefix(trimmedTag, "-u "))
			} else {
				unitName = strings.TrimSpace(strings.TrimPrefix(trimmedTag, "--unit "))
			}

			if unitName == "" {
				slog.Warn("Tag for unit filtering resulted in empty unit name",
					"check", check.Name,
					"tag", trimmedTag)

				continue
			}

			matchExpr := FieldSystemdUnit + "=" + unitName
			slog.Info("Adding unit filter",
				"check", check.Name,
				"tag", trimmedTag,
				"match", matchExpr)

			if err := journal.AddMatch(matchExpr); err != nil {
				return fmt.Errorf("check '%s': failed to add unit match for '%s' (using expression '%s'): %w",
					check.Name, unitName, matchExpr, err)
			}
		}
	}

	return nil
}

// processJournalEntries reads and processes journal entries.
func (sm *SyslogMonitor) processJournalEntries(journal Journal, check CheckDefinition) error {
	lastKnownCursor, hasLastCursor := sm.checkLastCursors[check.Name]

	bootID, err := journal.GetBootID()
	if err != nil {
		slog.Warn("Failed to get boot ID", "check", check.Name, "error", err)
	}

	slog.Info("Boot ID for check", "check", check.Name, "bootID", bootID)

	if !hasLastCursor {
		return sm.initializeJournalFromTail(journal, check)
	}

	ready, err := sm.resumeFromLastCursor(journal, check, lastKnownCursor)
	if err != nil {
		return err
	}

	if !ready {
		return nil
	}

	return sm.processAllEntries(journal, check)
}

// resumeFromLastCursor seeks to the last known cursor and advances to the first new entry.
// Returns (true, nil) when the journal is positioned at the first new entry to process;
// (false, nil) when there are no new entries or after re-initializing on seek failure;
// (false, err) on error.
func (sm *SyslogMonitor) resumeFromLastCursor(
	journal Journal, check CheckDefinition, lastKnownCursor string,
) (bool, error) {
	slog.Info("Resuming from last known cursor",
		"check", check.Name,
		"cursor", lastKnownCursor)

	if err := journal.SeekCursor(lastKnownCursor); err != nil {
		slog.Warn("Failed to seek to last known cursor, re-initializing",
			"check", check.Name,
			"cursor", lastKnownCursor,
			"error", err)

		if errSeekTail := journal.SeekTail(); errSeekTail != nil {
			return false, fmt.Errorf("check '%s': failed to seek to journal tail during "+
				"re-initialization after SeekCursor error: %w", check.Name, errSeekTail)
		}

		tailCursor, errGetCursor := journal.GetCursor()
		if errGetCursor != nil {
			return false, fmt.Errorf("check '%s': failed to get cursor at journal tail during re-initialization: %w",
				check.Name, errGetCursor)
		}

		slog.Info("Re-initialized journal processing",
			"check", check.Name,
			"cursor", tailCursor)

		sm.checkLastCursors[check.Name] = tailCursor

		return false, nil // No entries processed on this re-initialization run.
	}

	// Successfully sought to lastKnownCursor. Now advance to the *next* entry.
	// This is crucial: we process entries *after* the lastKnownCursor.
	advanced, nextErr := journal.Next()
	if nextErr != nil && !errors.Is(nextErr, io.EOF) {
		return false, fmt.Errorf("check '%s': error advancing from resumed cursor '%s': %w",
			check.Name, lastKnownCursor, nextErr)
	}

	if errors.Is(nextErr, io.EOF) || advanced == 0 {
		slog.Info("No new entries since last cursor",
			"check", check.Name,
			"cursor", lastKnownCursor)

		return false, nil
	}

	ready, err := sm.skipBookmarkedEntryIfPresent(journal, check, lastKnownCursor)
	if err != nil {
		return false, err
	}

	return ready, nil
}

// skipBookmarkedEntryIfPresent advances past lastKnownCursor when a filtered
// journal read returns the bookmarked entry itself after SeekCursor+Next.
// systemd may return the entry at the cursor on the first Next() after
// SeekCursor when journal matches (e.g. _TRANSPORT=kernel for "-k") are active.
// Without this skip, the last processed kernel XID is re-read every poll cycle.
func (sm *SyslogMonitor) skipBookmarkedEntryIfPresent(
	journal Journal, check CheckDefinition, lastKnownCursor string,
) (bool, error) {
	cur, err := journal.GetCursor()
	if err != nil {
		return false, fmt.Errorf("check '%s': failed to get cursor after resume advance: %w", check.Name, err)
	}

	if cur != lastKnownCursor {
		return true, nil
	}

	slog.Debug("Skipping bookmarked journal entry re-read after filtered resume",
		"check", check.Name,
		"cursor", lastKnownCursor)

	advanced, nextErr := journal.Next()
	if nextErr != nil && !errors.Is(nextErr, io.EOF) {
		return false, fmt.Errorf("check '%s': error advancing past bookmarked cursor '%s': %w",
			check.Name, lastKnownCursor, nextErr)
	}

	if errors.Is(nextErr, io.EOF) || advanced == 0 {
		slog.Info("No new entries since last cursor",
			"check", check.Name,
			"cursor", lastKnownCursor)

		return false, nil
	}

	return true, nil
}

// processAllEntries processes journal entries from the current cursor to the end.
// The journal must already be positioned at the first entry to process.
func (sm *SyslogMonitor) processAllEntries(journal Journal, check CheckDefinition) error {
	for {
		currentEntryCursor, err := journal.GetCursor()
		if err != nil {
			breakLoop, retErr := sm.recoverFromGetCursorError(journal, check)
			if retErr != nil {
				return retErr
			}

			if breakLoop {
				break
			}

			continue
		}

		message, err := sm.getJournalMessage(journal, check.Name)
		if err != nil {
			breakLoop, retErr := sm.recoverFromMessageError(journal, check, currentEntryCursor, err)
			if retErr != nil {
				return retErr
			}

			if breakLoop {
				break
			}

			continue
		}

		breakLoop, retErr := sm.processOneEntryAndAdvance(journal, check, currentEntryCursor, message)
		if retErr != nil {
			return retErr
		}

		if breakLoop {
			break
		}
	}

	finalCursor := sm.checkLastCursors[check.Name]
	slog.Info("Finished processing journal entries",
		"check", check.Name,
		"nextCursor", finalCursor)

	return nil
}

// recoverFromGetCursorError attempts to advance past the current entry after a GetCursor error.
// Returns (true, nil) if end of journal; (false, err) on advance error; (false, nil) to continue.
func (sm *SyslogMonitor) recoverFromGetCursorError(journal Journal, check CheckDefinition) (bool, error) {
	slog.Warn("Failed to get cursor for current entry, attempting to advance",
		"check", check.Name,
		"lastStoredCursor", sm.checkLastCursors[check.Name])

	advancedNext, advErr := journal.Next()
	if errors.Is(advErr, io.EOF) || advancedNext == 0 {
		slog.Info("Reached end of journal while recovering from GetCursor error",
			"check", check.Name,
			"nextCursor", sm.checkLastCursors[check.Name])

		return true, nil
	}

	if advErr != nil {
		slog.Error("Error advancing journal after GetCursor error, stopping",
			"check", check.Name,
			"error", advErr,
			"nextCursor", sm.checkLastCursors[check.Name])

		return false, fmt.Errorf("error advancing after GetCursor error for check '%s' "+
			"(last stored cursor for next run %s): %w",
			check.Name, sm.checkLastCursors[check.Name], advErr)
	}

	return false, nil
}

// recoverFromMessageError attempts to advance past the current entry after a getJournalMessage error.
// Returns (true, nil) if end of journal; (false, err) on advance error; (false, nil) to continue.
func (sm *SyslogMonitor) recoverFromMessageError(
	journal Journal, check CheckDefinition, currentEntryCursor string, messageErr error,
) (bool, error) {
	slog.Warn("Failed to get journal message, skipping entry",
		"check", check.Name,
		"cursor", currentEntryCursor,
		"error", messageErr,
		"nextCursor", sm.checkLastCursors[check.Name])

	advancedNext, advErr := journal.Next()
	if errors.Is(advErr, io.EOF) || advancedNext == 0 {
		slog.Info("Reached end of journal while recovering from message error",
			"check", check.Name,
			"entryCursor", currentEntryCursor,
			"nextCursor", sm.checkLastCursors[check.Name])

		return true, nil
	}

	if advErr != nil {
		slog.Error("Error advancing journal after message error, stopping",
			"check", check.Name,
			"entryCursor", currentEntryCursor,
			"error", advErr,
			"nextCursor", sm.checkLastCursors[check.Name])

		return false, fmt.Errorf("error advancing after getJournalMessage for check '%s' "+
			"(entry cursor %s, last stored cursor for next run %s): %v",
			check.Name, currentEntryCursor, sm.checkLastCursors[check.Name], advErr)
	}

	return false, nil
}

// processOneEntryAndAdvance updates cursor for the entry, handles the message, and advances.
// Returns (true, nil) if end of journal; (false, err) on advance error; (false, nil) to continue.
func (sm *SyslogMonitor) processOneEntryAndAdvance(
	journal Journal, check CheckDefinition, currentEntryCursor string, message string,
) (bool, error) {
	if message == "" {
		sm.checkLastCursors[check.Name] = currentEntryCursor
		slog.Info("Check, read empty message", "name", check.Name,
			"message", message,
			"cursor", currentEntryCursor)
	} else {
		err := sm.handleSingleLine(check, message)
		if err != nil {
			// Skip this entry on handler error; continue processing remaining entries.
			return false, nil //nolint:nilerr // intentional: do not stop the loop
		}

		sm.checkLastCursors[check.Name] = currentEntryCursor
		slog.Debug("Check errored but considered processed", "name", check.Name,
			"message", message,
			"cursor", currentEntryCursor)
	}

	advancedNext, advErr := journal.Next()
	if errors.Is(advErr, io.EOF) || advancedNext == 0 {
		slog.Info("Check no more", "name", check.Name, "cursor", currentEntryCursor)

		return true, nil
	}

	if advErr != nil {
		slog.Error("Error reading next journal entry, stopping",
			"check", check.Name,
			"cursor", currentEntryCursor,
			"error", advErr)

		return false, fmt.Errorf("check '%s': error reading next journal entry after cursor %s: %w",
			check.Name, currentEntryCursor, advErr)
	}

	return false, nil
}

// initializeJournalFromTail seeks to the journal tail, sets the resume cursor, and returns.
// Used when there is no last known cursor (first run or no saved state). Returns nil on success.
func (sm *SyslogMonitor) initializeJournalFromTail(journal Journal, check CheckDefinition) error {
	slog.Info("No last known cursor, seeking to journal tail", "check", check.Name)

	if err := journal.SeekTail(); err != nil {
		return fmt.Errorf("check '%s': failed to seek to journal tail for initialization: %w", check.Name, err)
	}

	count, errPrev := journal.Previous()
	if errPrev != nil && !errors.Is(errPrev, io.EOF) {
		return fmt.Errorf("seek previous: %w", errPrev)
	}

	if count == 0 {
		slog.Info("Journal is empty, nothing to do", "check", check.Name)

		return nil
	}

	cursor, err := journal.GetCursor()
	if err != nil {
		if isRetryableJournalError(err) {
			slog.Warn("Transient journal read error, will retry on next run",
				"check", check.Name,
				"error", err)

			return nil
		}

		return fmt.Errorf("get cursor: %w", err)
	}

	slog.Info("Initialized. Journal processing will start from entries after cursor on the next run",
		"check", check.Name,
		"cursor", cursor)

	sm.checkLastCursors[check.Name] = cursor

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
		ProcessingStrategy: sm.processingStrategy,
	}

	return &pb.HealthEvents{
		Version: 1,
		Events:  []*pb.HealthEvent{event},
	}
}

// sendHealthEventWithRetry forwards health events via the shared
// healthpub publisher. The publisher is built per call so tests that
// swap sm.pcClient after construction take effect.
func (sm *SyslogMonitor) sendHealthEventWithRetry(healthEvents *pb.HealthEvents,
	maxRetries int, retryDelay time.Duration) error {
	slog.Info("Attempting to send health event", "events", healthEvents)

	pub := healthpub.New(sm.pcClient, sm.platformConnectorTarget, sm.defaultAgentName,
		healthpub.WithRetryPolicy(maxRetries, retryDelay, 1.5, 0.1))

	if err := pub.Publish(context.Background(), healthEvents); err != nil {
		if errors.Is(err, healthpub.ErrPlatformConnectorUnavailable) {
			slog.Warn("Skipped health event send: platform-connector unavailable. "+
				"Next poll will re-evaluate and re-stamp.",
				"events", healthEvents)

			return fmt.Errorf("failed all attempts to send health events: %w", err)
		}

		slog.Error("All retry attempts to send health event failed", "error", err)

		return fmt.Errorf("failed all attempts to send health events: %w", err)
	}

	slog.Info("Successfully sent health events", "events", healthEvents)

	return nil
}

func (sm *SyslogMonitor) handleSingleLine(check CheckDefinition, lineToEvaluate string) error {
	if handler, ok := sm.checkToHandlerMap[check.Name]; ok {
		healthEvents, err := handler.ProcessLine(lineToEvaluate)
		if err != nil {
			return fmt.Errorf("error processing line %s: %w", lineToEvaluate, err)
		}

		if healthEvents != nil {
			if err := sm.sendHealthEventWithRetry(healthEvents, 5, 2*time.Second); err != nil {
				return fmt.Errorf("failed to send health event: %w", err)
			}
		}
	}

	return nil
}
