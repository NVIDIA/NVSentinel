// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package verdict holds the part of NVHV's node verdict the monitor reads, as
// plain Go values. The nvhvclient package fills it from NVHV's own schema, so
// the monitor carries no copy of NVHV's proto.
package verdict

// Severity is the name of a value of NVHV's severity enum.
type Severity string

// The severities the monitor acts on. NVHV may add others; the monitor treats an
// unknown one as no fault.
const (
	Pass     Severity = "PASS"
	Info     Severity = "INFO"
	Warning  Severity = "WARNING"
	Unknown  Severity = "UNKNOWN"
	Critical Severity = "CRITICAL"
)

// Verdict is one node verdict.
type Verdict struct {
	SchemaVersion string
	Results       []Result
}

// Result is one check's result in a verdict.
type Result struct {
	CheckID  string
	Severity Severity
	// Category is the name of NVHV's category enum value, such as "xid" or "gpu".
	Category         string
	Skipped          bool
	Messages         []string
	AffectedEntities []string
	// OperationalState is "ready", "monitor" or "drain".
	OperationalState   string
	RemediationActions []string
	// ObservedStrings holds the observed values that are strings, by name.
	ObservedStrings map[string]string
}
