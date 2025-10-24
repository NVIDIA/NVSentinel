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
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/syslog-health-monitor/pkg/types"
)

const (
	// Version of the state file format
	stateFileVersion = 1
)

// Field constants for journal entries
const (
	// Boot ID field in journal entries (underscore-prefixed in real journal).
	FieldBootID         = "_BOOT_ID"
	FieldMessage        = "MESSAGE"
	FieldSyslogFacility = "SYSLOG_FACILITY"
	FieldSystemdUnit    = "_SYSTEMD_UNIT"

	XIDErrorCheck  = "SysLogsXIDError"
	SXIDErrorCheck = "SysLogsSXIDError"
)

// syslogMonitorState represents the persistent state of the syslog monitor
type syslogMonitorState struct {
	Version          int               `json:"version"`
	BootID           string            `json:"boot_id"`
	CheckLastCursors map[string]string `json:"check_last_cursors"`
}

// SyslogMonitor monitors journal logs for error patterns
type SyslogMonitor struct {
	nodeName              string
	checks                []CheckDefinition
	pcClient              pb.PlatformConnectorClient
	defaultAgentName      string
	defaultComponentClass string
	pollingInterval       string
	// Map of check name to last processed cursor
	checkLastCursors map[string]string
	// Factory for creating Journal instances
	journalFactory JournalFactory
	// Current system boot ID
	currentBootID string
	// Path to state file for persistence
	stateFilePath string
	// Map of check name to handler
	checkToHandlerMap map[string]types.Handler
	// Endpoint to the XID analyser service
	xidAnalyserEndpoint string
}

// CheckDefinition matches the structure of each check in the YAML config file
type CheckDefinition struct {
	Name        string   `yaml:"name"`
	Tags        []string `yaml:"tags"`
	JournalPath string   `yaml:"journalPath"`
}
