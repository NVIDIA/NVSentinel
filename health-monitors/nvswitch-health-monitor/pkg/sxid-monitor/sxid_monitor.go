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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"k8s.io/klog"
)

const (
	SYSLOG_ACTION_READ_ALL    = 3
	SYSLOG_ACTION_SIZE_BUFFER = 10

	// current state file version
	stateFileVersion = 1
)

// kernel log starts with this timestamp format (e.g. <12>[73309.599396])
var logPrefixPattern = regexp.MustCompile(`^<\d+>\[\s*(\d+\.\d+)\s*\]`)

var storedBootID string

type nvSwitchMonitorState struct {
	Version       int     `json:"version"`
	LastTimestamp float64 `json:"last_timestamp"`
	LastLogLine   string  `json:"last_log_line"`
	BootID        string  `json:"boot_id"`
}

func saveState(stateFilePath string, state nvSwitchMonitorState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(stateFilePath), 0755); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	if err := os.WriteFile(stateFilePath, data, 0600); err != nil {
		return fmt.Errorf("failed to write state to file: %w", err)
	}

	return nil
}

func loadState(stateFilePath string) (nvSwitchMonitorState, error) {
	var state nvSwitchMonitorState

	data, err := os.ReadFile(stateFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, fmt.Errorf("failed to read state from file: %w", err)
	}

	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	if state.Version != 0 && state.Version != stateFileVersion {
		if verifyIfNecessaryFieldsForCurrentStateVersionArePresent(state) {
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

	return state, nil
}

func verifyIfNecessaryFieldsForCurrentStateVersionArePresent(state nvSwitchMonitorState) bool {
	if state.BootID == "" || state.LastLogLine == "" || state.LastTimestamp == 0.0 {
		return false
	}

	return true
}

func fetchCurrentBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("failed to read boot_id: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

type SxidEventMonitorConfig struct {
	StateFilePath                 string
	PollingIntervalInMilliseconds int
}

type SxidEventMonitor struct {
	EventChan                     chan *SXIDErrorEvent
	lastTimestamp                 float64
	lastLogLine                   string
	stateFilePath                 string
	pollingIntervalInMilliseconds int
}

func NewSxidEventMonitor(config *SxidEventMonitorConfig) (*SxidEventMonitor, error) {
	state, err := loadState(config.StateFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	return &SxidEventMonitor{
		EventChan:                     make(chan *SXIDErrorEvent),
		lastTimestamp:                 state.LastTimestamp,
		lastLogLine:                   state.LastLogLine,
		stateFilePath:                 config.StateFilePath,
		pollingIntervalInMilliseconds: config.PollingIntervalInMilliseconds,
	}, nil
}

func (c *SxidEventMonitor) Close() {
	close(c.EventChan)
}

func (c *SxidEventMonitor) Run() error {
	currentBootID, err := fetchCurrentBootID()
	if err != nil {
		klog.Fatalf("error fetching current bootID: %v", err)
	}

	// store the currentBootID locally so that we can refer to it
	storedBootID = currentBootID

	// load existing state
	state, err := loadState(c.stateFilePath)
	if err != nil {
		klog.Fatalf("error loading state: %v", err)
	}

	if err = c.compareBootIDAndEmitHealthyEventIfChanged(state, currentBootID); err != nil {
		klog.Fatalf("error comparing bootID: %v", err)
	}

	klog.Infof("Collecting SXid events from syslog")

	// get the total size of the kernel log buffer
	size, _, errno := syscall.Syscall(syscall.SYS_SYSLOG, SYSLOG_ACTION_SIZE_BUFFER, 0, 0)
	if errno != 0 {
		return fmt.Errorf("failed to get buffer size: %w", errno)
	}

	klog.Infof("Total buffer size: %d bytes", size)

	buffer := make([]byte, size)

	pollingInterval := time.Duration(c.pollingIntervalInMilliseconds) * time.Millisecond

	for {
		start := time.Now()
		readSize, err := readKernelLog(buffer, size)
		if err != nil {
			klog.Errorf("error while reading kernel log buffer: %v", err)
			time.Sleep(pollingInterval)
			continue
		}

		// process the new kernel log messages
		if readSize > 0 {
			logs := string(buffer[:readSize])
			lines := strings.Split(logs, "\n")

			for _, log := range lines {
				if err := c.processLog(log); err != nil {
					klog.Errorf("error while processing log line %s: %v", log, err)
					continue
				}
			}
		}

		duration := float64(time.Since(start).Milliseconds())
		pollingLoopProcessingDuration.Observe(duration)
		time.Sleep(pollingInterval)
	}
}

func (c *SxidEventMonitor) compareBootIDAndEmitHealthyEventIfChanged(state nvSwitchMonitorState,
	currentBootID string) error {
	if state.BootID != currentBootID {
		klog.Infof("Detected bootID change. Old bootID: %s, New bootID: %s", state.BootID, currentBootID)

		// emit healthy event
		healthyEvent := &SXIDErrorEvent{
			IsFatal:   false,
			IsHealthy: true,
			Message:   fmt.Sprintf("System reboot detected. BootID changed from %s to %s", state.BootID, currentBootID),
		}
		c.EventChan <- healthyEvent

		// update state with new bootID
		state.BootID = currentBootID
		state.Version = stateFileVersion

		if err := saveState(c.stateFilePath, state); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	return nil
}

// read the entire kernel log buffer non-destructively
func readKernelLog(buffer []byte, size uintptr) (int, error) {
	syslogReadCallsSucceeded.Inc()
	readSize, _, errno := syscall.Syscall(syscall.SYS_SYSLOG,
		SYSLOG_ACTION_READ_ALL, uintptr(unsafe.Pointer(&buffer[0])), size)
	if errno != 0 {
		syslogReadCallsFailed.Inc()
		return 0, fmt.Errorf("failed to read kernel log buffer: %w", errno)
	}
	//nolint:gosec // G115: integer overflow conversion uintptr -> int
	return int(readSize), nil
}

// process a log line from the kernel log buffer
func (c *SxidEventMonitor) processLog(log string) error {
	if log == "" {
		return nil
	}
	kernelLogsProcessed.Inc()

	timestamp, err := extractTimestamp(log)
	if err != nil {
		return err
	}

	// process log entries with a timestamp greater than the last processed timestamp
	// or with the same timestamp but a different log line to avoid skipping different logs which
	// have the same timestamp
	//nolint
	if timestamp > c.lastTimestamp || (timestamp == c.lastTimestamp && log != c.lastLogLine) {
		m, err := ParseSXIDError(log)
		if err != nil {
			sxidLogsProcessingFailed.Inc()

			// We should record the parse error and should mark this log line as processed
			parseErr := fmt.Errorf("failed to parse SXID error: %w", err)

			c.lastTimestamp = timestamp
			c.lastLogLine = log

			if err := saveState(c.stateFilePath, nvSwitchMonitorState{
				Version:       stateFileVersion,
				LastTimestamp: timestamp,
				LastLogLine:   log,
				BootID:        storedBootID,
			}); err != nil {
				return fmt.Errorf("failed to save state: %w", err)
			}

			return parseErr
		}

		if m != nil {
			c.EventChan <- m
			sxidLogsProcessingSucceeded.Inc()
		}

		c.lastTimestamp = timestamp
		c.lastLogLine = log
		// save state in file after processing each log line
		if err := saveState(c.stateFilePath, nvSwitchMonitorState{
			Version:       stateFileVersion,
			LastTimestamp: timestamp,
			LastLogLine:   log,
			BootID:        storedBootID,
		}); err != nil {
			return fmt.Errorf("failed to save state: %w", err)
		}
	}

	return nil
}

// extract the timestamp as a float from the kernel log line
func extractTimestamp(log string) (float64, error) {
	matches := logPrefixPattern.FindStringSubmatch(log)
	if len(matches) != 2 {
		return 0, fmt.Errorf("failed to extract timestamp from log: %s", log)
	}

	timestamp, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse timestamp: %w", err)
	}
	return timestamp, nil
}

type SXIDErrorEvent struct {
	ErrorNum  int
	IsFatal   bool
	IsHealthy bool
	NVSwitch  int
	PCI       string
	Link      int
	Message   string
}

// nolint:cyclop
// most of complexity comes from error checking. so we skip cyclomatic check here
func ParseSXIDError(str string) (*SXIDErrorEvent, error) {
	const (
		// index for each token of the log
		// examples of logs:
		// [38889.018130] nvidia-nvswitch1: SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04 RX Short Error Rate
		// [ 1108.858286] nvidia-nvswitch0: SXid (PCI:0000:c3:00.0): 24007, Fatal, Link 28 sourcetrack timeout error (First)

		// old log type with no Link info we want to ignore
		// [38889.018130] nvidia-nvswitch0: SXid (PCI:0000:07:00.0): 10001, Non-fatal, PRI WRITE SYSB error, instance=3
		NvswitchX       = 0 // Index for 'nvidia-nvswitchX:'
		SXidConst       = 1 // Index for 'SXid'
		pciIdx          = 2 // Index for PCI information
		errorNumIdx     = 3 // Index for error number
		fatalIdx        = 4 // Index for Fatal/Non-fatal
		linkStrIdx      = 5 // Index for 'Link' keyword
		linkIdx         = 6 // Index for link number
		messageStartIdx = 7 // Index for error message
	)

	// remove the kernel log prefix
	syslogPrefixPattern := regexp.MustCompile(`^(<\d+>)?\[\s*(\d+\.\d+)\s*\]`)
	str = syslogPrefixPattern.ReplaceAllString(str, "")

	words := strings.Fields(str)
	if len(words) <= fatalIdx {
		// log does not have enough information to check if the log is for SXid, so skip.
		return nil, nil
	}

	// Check for 'nvidia-nvswitchX:' and 'SXid'
	if !strings.HasPrefix(words[NvswitchX], "nvidia-nvswitch") || !strings.HasSuffix(words[NvswitchX], ":") ||
		words[SXidConst] != "SXid" {
		// Not a relevant SXid log, skip
		return nil, nil
	}

	if len(words) <= linkIdx {
		return nil, fmt.Errorf("log message truncated: %s", str)
	}

	// Parse NVSwitch number
	nvswitchStr := strings.TrimPrefix(strings.TrimSuffix(words[NvswitchX], ":"), "nvidia-nvswitch")
	nvswitch, err := strconv.Atoi(nvswitchStr)
	if err != nil {
		return nil, fmt.Errorf("cannot parse nvidia-nvswitch number: %s", nvswitchStr)
	}

	// Parse PCI address
	pci := strings.TrimSuffix(strings.TrimPrefix(words[pciIdx], "(PCI:"), "):")

	// Parse error number
	errorNumStr := strings.TrimSuffix(words[errorNumIdx], ",")
	errorNum, err := strconv.Atoi(errorNumStr)
	if err != nil {
		return nil, fmt.Errorf("cannot parse error number: %s", errorNumStr)
	}

	// Parse Fatal/Non-fatal
	var isFatal bool
	switch {
	case strings.HasPrefix(words[fatalIdx], "Fatal"):
		isFatal = true
	case strings.HasPrefix(words[fatalIdx], "Non-fatal"):
		isFatal = false
	default:
		// it is a continuing error line, so we don't report metric
		return nil, nil
	}

	// expect the link information to be present, else return error
	if len(words) < linkStrIdx || words[linkStrIdx] != "Link" {
		return nil, errors.New("link information is missing")
	}

	var link int
	// expect the link number to be present, else return error
	if len(words) > linkIdx {
		// parse Link number
		link, err = strconv.Atoi(words[linkIdx])
		if err != nil {
			return nil, fmt.Errorf("cannot parse link number: %s", words[linkIdx])
		}
	} else {
		// Some old log has missing Link information - we'd like report this as error and monitor
		return nil, errors.New("link information is missing")
	}

	// Extract error message
	errorMessage := ""
	if len(words)+1 >= messageStartIdx {
		errorMessage = strings.Join(words[messageStartIdx:], " ")
	}

	return &SXIDErrorEvent{
		ErrorNum: errorNum,
		IsFatal:  isFatal,
		NVSwitch: nvswitch,
		PCI:      pci,
		Link:     link,
		Message:  errorMessage,
	}, nil
}
