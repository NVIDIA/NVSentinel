// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build nvml

package nvml

// XID errors documentation:
// https://docs.nvidia.com/deploy/xid-errors/index.html

// DefaultIgnoredXids contains XID error codes that are typically caused by
// application errors rather than hardware failures. These are ignored by
// default to avoid false positives in health monitoring.
//
// Reference: https://docs.nvidia.com/deploy/xid-errors/index.html#topic_4
var DefaultIgnoredXids = map[uint64]bool{
	// Application errors - GPU should still be healthy
	13:  true, // Graphics Engine Exception
	31:  true, // GPU memory page fault
	43:  true, // GPU stopped processing
	45:  true, // Preemptive cleanup, due to previous errors
	68:  true, // Video processor exception
	109: true, // Context Switch Timeout Error
}

// CriticalXids contains XID error codes that indicate critical hardware
// failures requiring immediate attention.
var CriticalXids = map[uint64]bool{
	// Memory errors
	48: true, // Double Bit ECC Error
	63: true, // Row remapping failure
	64: true, // Uncontained ECC error
	74: true, // NVLink error
	79: true, // GPU has fallen off the bus

	// Fatal errors
	94:  true, // Contained ECC error (severe)
	95:  true, // Uncontained ECC error
	119: true, // GSP (GPU System Processor) error
	120: true, // GSP firmware error
}

// XidDescriptions provides human-readable descriptions for common XIDs.
var XidDescriptions = map[uint64]string{
	// Application errors (typically ignored)
	13:  "Graphics Engine Exception",
	31:  "GPU memory page fault",
	43:  "GPU stopped processing",
	45:  "Preemptive cleanup",
	68:  "Video processor exception",
	109: "Context Switch Timeout",

	// Memory errors
	48: "Double Bit ECC Error",
	63: "Row remapping failure",
	64: "Uncontained ECC error",
	74: "NVLink error",
	79: "GPU has fallen off the bus",
	94: "Contained ECC error",
	95: "Uncontained ECC error",

	// Other notable XIDs
	8:   "GPU not accessible",
	32:  "Invalid or corrupted push buffer stream",
	38:  "Driver firmware error",
	56:  "Display engine error",
	57:  "Error programming video memory interface",
	62:  "Internal micro-controller halt (non-fatal)",
	69:  "Graphics engine accessor error",
	119: "GSP error",
	120: "GSP firmware error",
}

// isIgnoredXid returns true if the XID should be ignored for health purposes.
//
// An XID is ignored if it's in the default ignored list OR in the additional
// ignored list provided by the user.
func isIgnoredXid(xid uint64, additionalIgnored []uint64) bool {
	// Check default ignored list
	if DefaultIgnoredXids[xid] {
		return true
	}

	// Check additional ignored list
	for _, ignoredXid := range additionalIgnored {
		if xid == ignoredXid {
			return true
		}
	}

	return false
}

// IsCriticalXid returns true if the XID indicates a critical hardware failure.
func IsCriticalXid(xid uint64) bool {
	return CriticalXids[xid]
}

// xidToString returns a human-readable description for an XID.
func xidToString(xid uint64) string {
	if desc, ok := XidDescriptions[xid]; ok {
		return desc
	}

	return "Unknown XID"
}

// ParseIgnoredXids parses a comma-separated string of XID values.
//
// Invalid values are skipped with a warning log.
func ParseIgnoredXids(input string) []uint64 {
	if input == "" {
		return nil
	}

	var (
		result   []uint64
		current  uint64
		hasDigit bool
	)

	for _, ch := range input {
		switch {
		case ch >= '0' && ch <= '9':
			current = current*10 + uint64(ch-'0')
			hasDigit = true
		case ch == ',' || ch == ' ':
			if hasDigit {
				result = append(result, current)
				current = 0
				hasDigit = false
			}
		}
	}

	// Don't forget the last value
	if hasDigit {
		result = append(result, current)
	}

	return result
}

// XidSeverity represents the severity level of an XID error.
type XidSeverity int

const (
	// XidSeverityUnknown indicates the XID severity is unknown.
	XidSeverityUnknown XidSeverity = iota
	// XidSeverityIgnored indicates the XID is typically caused by applications.
	XidSeverityIgnored
	// XidSeverityWarning indicates the XID may indicate a problem.
	XidSeverityWarning
	// XidSeverityCritical indicates the XID indicates a critical hardware failure.
	XidSeverityCritical
)

// Severity string constants.
const (
	severityUnknown  = "unknown"
	severityIgnored  = "ignored"
	severityWarning  = "warning"
	severityCritical = "critical"
)

// GetXidSeverity returns the severity level for an XID.
func GetXidSeverity(xid uint64) XidSeverity {
	if DefaultIgnoredXids[xid] {
		return XidSeverityIgnored
	}

	if CriticalXids[xid] {
		return XidSeverityCritical
	}

	// XIDs not in either list are treated as warnings
	return XidSeverityWarning
}

// String returns a string representation of XidSeverity.
func (s XidSeverity) String() string {
	switch s {
	case XidSeverityUnknown:
		return severityUnknown
	case XidSeverityIgnored:
		return severityIgnored
	case XidSeverityWarning:
		return severityWarning
	case XidSeverityCritical:
		return severityCritical
	default:
		return severityUnknown
	}
}
