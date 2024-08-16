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
)

// kernel log starts with this timestamp format (e.g. <12>[73309.599396])
var logPrefixPattern = regexp.MustCompile(`^<\d+>\[\s*(\d+\.\d+)\s*\]`)

type nvSwitchMonitorState struct {
	LastTimestamp float64 `json:"last_timestamp"`
	LastLogLine   string  `json:"last_log_line"`
}

func saveState(stateFilePath string, state nvSwitchMonitorState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
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

	return state, nil
}

type SxidErrorMonitor struct {
	EventChan     chan *SXIDErrorEvent
	lastTimestamp float64
	lastLogLine   string
	stateFilePath string
}

func NewSxidErrorMonitor(stateFilePath string) (*SxidErrorMonitor, error) {
	state, err := loadState(stateFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load state: %w", err)
	}

	return &SxidErrorMonitor{
		EventChan:     make(chan *SXIDErrorEvent),
		lastTimestamp: state.LastTimestamp,
		lastLogLine:   state.LastLogLine,
		stateFilePath: stateFilePath,
	}, nil
}

func (c *SxidErrorMonitor) Close() {
	close(c.EventChan)
}

func (c *SxidErrorMonitor) Run() error {
	klog.Infof("Collecting SXid events from syslog")

	// get the total size of the kernel log buffer
	size, _, errno := syscall.Syscall(syscall.SYS_SYSLOG, SYSLOG_ACTION_SIZE_BUFFER, 0, 0)
	if errno != 0 {
		return fmt.Errorf("failed to get buffer size: %w", errno)
	}

	klog.Infof("Total buffer size: %d bytes", size)

	buffer := make([]byte, size)

	pollingInterval := 100 * time.Millisecond

	for {
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

		time.Sleep(pollingInterval)
	}
}

// read the entire kernel log buffer non-destructively
func readKernelLog(buffer []byte, size uintptr) (int, error) {
	readSize, _, errno := syscall.Syscall(syscall.SYS_SYSLOG,
		SYSLOG_ACTION_READ_ALL, uintptr(unsafe.Pointer(&buffer[0])), size)
	if errno != 0 {
		return 0, fmt.Errorf("failed to read kernel log buffer: %w", errno)
	}
	return int(readSize), nil
}

// process a log line from the kernel log buffer
func (c *SxidErrorMonitor) processLog(log string) error {
	if log == "" {
		return nil
	}

	timestamp, err := extractTimestamp(log)
	if err != nil {
		return err
	}

	// process log entries with a timestamp greater than the last processed timestamp
	// or with the same timestamp but a different log line to avoid skipping different logs which
	// have the same timestamp
	if timestamp > c.lastTimestamp || (timestamp == c.lastTimestamp && log != c.lastLogLine) {
		m, err := ParseSXIDError(log)
		if err != nil {
			return fmt.Errorf("failed to parse SXID error: %w", err)
		}

		if m != nil {
			klog.Info(m)
			c.EventChan <- m
		}

		c.lastTimestamp = timestamp
		c.lastLogLine = log
		// save state in file after processing each log line
		if err := saveState(c.stateFilePath, nvSwitchMonitorState{
			LastTimestamp: timestamp,
			LastLogLine:   log,
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
	ErrorNum int
	IsFatal  bool
	NVSwitch int
	PCI      string
	Link     int
	Message  string
}

// nolint:cyclop
// most of complexity comes from error checking. so we skip cyclomatic check here
func ParseSXIDError(str string) (*SXIDErrorEvent, error) {
	const (
		// index for each token of the log
		// PRIORITY,SEQUENCE_NUM,TIMESTAMP,-;MESSAGE
		// MESSAGE := nvidia-nvswitch1 SXid (PCI:0000:06:00.0): 20009, Non-fatal, Link 04\n"
		NvswitchX   = 0
		SXidConst   = 1
		pciIdx      = 2
		errorNumIdx = 3
		fatalIdx    = 4
		linkStrIdx  = 5
		linkIdx     = 6
	)

	// remove the kernel log prefix
	logPrefixPattern := regexp.MustCompile(`^<\d+>\[\d+\.\d+\] `)
	str = logPrefixPattern.ReplaceAllString(str, "")

	words := strings.Fields(str)

	if len(words) <= SXidConst {
		// log does not have enough information to check if the log is for SXid, so skip.
		return nil, nil
	}

	if !strings.Contains(words[NvswitchX], "nvidia-nvswitch") || words[SXidConst] != "SXid" {
		// skip for non SXId log
		return nil, nil
	}

	if len(words) <= linkIdx {
		return nil, fmt.Errorf("log message truncated: %s", str)
	}

	kmsgMsgIdx := strings.Index(words[NvswitchX], ";")

	nvswitchStr := strings.TrimPrefix(words[NvswitchX][kmsgMsgIdx+1:], "nvidia-nvswitch")
	nvswitch, err := strconv.Atoi(nvswitchStr)
	if err != nil {
		return nil, fmt.Errorf("cannot parse nvidia-nvswitch %s", words[NvswitchX][kmsgMsgIdx:])
	}

	pci := words[pciIdx]
	pci = strings.TrimPrefix(pci, "(PCI:")
	pci = strings.TrimSuffix(pci, "):")

	errorNumStr := words[errorNumIdx]
	errorNumStr = strings.TrimSuffix(errorNumStr, ",")
	errorNum, err := strconv.Atoi(errorNumStr)
	if err != nil {
		return nil, fmt.Errorf("wrong errorNum %s", words[errorNumIdx])
	}

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

	// The first log of each SXid incidents must have "Fatal" | "Non-fatal" information

	if words[linkStrIdx] != "Link" {
		// Some old log has missing Link information - we'd like report this as error and monitor
		return nil, errors.New("link information is missing")
	}

	link, err := strconv.Atoi(words[linkIdx])
	if err != nil {
		return nil, fmt.Errorf("cannot parse link %s", words[linkIdx])
	}

	m := SXIDErrorEvent{
		NVSwitch: nvswitch,
		PCI:      pci,
		ErrorNum: errorNum,
		IsFatal:  isFatal,
		Link:     link,
		Message:  str[kmsgMsgIdx+1:],
	}

	return &m, nil
}
