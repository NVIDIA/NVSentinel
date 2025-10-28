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

package gpufallen

import (
	"context"
	"regexp"
	"sync"
	"time"
)

var (
	// Pattern to match GPU falling off the bus errors
	// This is most likely a single journal line that may contain newlines within it
	// Example: "[ 1843.308145] NVRM: The NVIDIA GPU 0000:b3:00.0\n"
	//          "               NVRM: (PCI ID: 10de:26b5) installed in this system has\n"
	//          "               NVRM: fallen off the bus and is not responding to commands."
	// The pattern uses (?s) flag to match across newlines within the single journal entry
	reGPUFallenPattern = regexp.MustCompile(
		`(?s)NVRM: The NVIDIA GPU ([0-9a-fA-F:.]+).*?fallen off the bus and is not responding to commands`)

	// Pattern to extract PCI ID from the full error message
	rePCIIDPattern = regexp.MustCompile(`PCI ID: ([0-9a-fA-F:]+)`)
)

// xidRecord tracks when an XID error was seen for a PCI address
type xidRecord struct {
	timestamp time.Time
	xidCode   int
}

// GPUFallenHandler processes syslog lines related to GPU fallen off bus errors.
type GPUFallenHandler struct {
	nodeName              string
	defaultAgentName      string
	defaultComponentClass string
	checkName             string
	mu                    sync.RWMutex
	recentXIDs            map[string]xidRecord // pciAddr -> XID record
	xidWindow             time.Duration        // how long to remember XID errors
	cancelCleanup         context.CancelFunc   // stops the cleanup goroutine
}

// gpuFallenErrorEvent represents a parsed GPU fallen off bus error event
type gpuFallenErrorEvent struct {
	pciAddr string
	pciID   string
	message string
}
