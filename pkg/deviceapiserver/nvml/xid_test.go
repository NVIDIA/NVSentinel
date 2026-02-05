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

import (
	"testing"
)

func TestIsIgnoredXid_DefaultIgnored(t *testing.T) {
	// Test default ignored XIDs
	defaultIgnored := []uint64{13, 31, 43, 45, 68, 109}

	for _, xid := range defaultIgnored {
		if !isIgnoredXid(xid, nil) {
			t.Errorf("XID %d should be ignored by default", xid)
		}
	}
}

func TestIsIgnoredXid_CriticalNotIgnored(t *testing.T) {
	// Test critical XIDs are not ignored by default
	criticalXids := []uint64{48, 63, 64, 74, 79, 94, 95, 119, 120}

	for _, xid := range criticalXids {
		if isIgnoredXid(xid, nil) {
			t.Errorf("Critical XID %d should not be ignored by default", xid)
		}
	}
}

func TestIsIgnoredXid_AdditionalIgnored(t *testing.T) {
	// Test additional ignored XIDs
	additionalIgnored := []uint64{48, 63} // Make critical XIDs ignored

	// Normally critical, but now ignored
	if !isIgnoredXid(48, additionalIgnored) {
		t.Error("XID 48 should be ignored when in additional list")
	}

	if !isIgnoredXid(63, additionalIgnored) {
		t.Error("XID 63 should be ignored when in additional list")
	}

	// Still critical (not in additional list)
	if isIgnoredXid(64, additionalIgnored) {
		t.Error("XID 64 should not be ignored (not in additional list)")
	}
}

func TestIsIgnoredXid_UnknownXid(t *testing.T) {
	// Unknown XIDs should not be ignored
	unknownXids := []uint64{1, 2, 3, 999, 12345}

	for _, xid := range unknownXids {
		if isIgnoredXid(xid, nil) {
			t.Errorf("Unknown XID %d should not be ignored", xid)
		}
	}
}

func TestIsCriticalXid(t *testing.T) {
	tests := []struct {
		xid      uint64
		expected bool
	}{
		// Critical XIDs
		{48, true},
		{63, true},
		{64, true},
		{74, true},
		{79, true},
		{94, true},
		{95, true},
		{119, true},
		{120, true},

		// Non-critical XIDs
		{13, false},
		{31, false},
		{43, false},
		{1, false},
		{999, false},
	}

	for _, tt := range tests {
		result := IsCriticalXid(tt.xid)
		if result != tt.expected {
			t.Errorf("IsCriticalXid(%d) = %v, want %v", tt.xid, result, tt.expected)
		}
	}
}

func TestXidToString(t *testing.T) {
	tests := []struct {
		xid      uint64
		expected string
	}{
		{13, "Graphics Engine Exception"},
		{31, "GPU memory page fault"},
		{48, "Double Bit ECC Error"},
		{79, "GPU has fallen off the bus"},
		{109, "Context Switch Timeout"},
		{999, "Unknown XID"},
		{0, "Unknown XID"},
	}

	for _, tt := range tests {
		result := xidToString(tt.xid)
		if result != tt.expected {
			t.Errorf("xidToString(%d) = %q, want %q", tt.xid, result, tt.expected)
		}
	}
}

func TestParseIgnoredXids(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []uint64
	}{
		{
			name:     "empty string",
			input:    "",
			expected: nil,
		},
		{
			name:     "single value",
			input:    "48",
			expected: []uint64{48},
		},
		{
			name:     "multiple comma separated",
			input:    "48,63,64",
			expected: []uint64{48, 63, 64},
		},
		{
			name:     "with spaces",
			input:    "48, 63, 64",
			expected: []uint64{48, 63, 64},
		},
		{
			name:     "space separated",
			input:    "48 63 64",
			expected: []uint64{48, 63, 64},
		},
		{
			name:     "mixed separators",
			input:    "48, 63 64,65",
			expected: []uint64{48, 63, 64, 65},
		},
		{
			name:     "trailing comma",
			input:    "48,63,",
			expected: []uint64{48, 63},
		},
		{
			name:     "leading comma",
			input:    ",48,63",
			expected: []uint64{48, 63},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseIgnoredXids(tt.input)

			if len(result) != len(tt.expected) {
				t.Errorf("ParseIgnoredXids(%q) len = %d, want %d", tt.input, len(result), len(tt.expected))
				return
			}

			for i, v := range result {
				if v != tt.expected[i] {
					t.Errorf("ParseIgnoredXids(%q)[%d] = %d, want %d", tt.input, i, v, tt.expected[i])
				}
			}
		})
	}
}

func TestGetXidSeverity(t *testing.T) {
	tests := []struct {
		xid      uint64
		expected XidSeverity
	}{
		// Ignored (application errors)
		{13, XidSeverityIgnored},
		{31, XidSeverityIgnored},
		{43, XidSeverityIgnored},
		{45, XidSeverityIgnored},
		{68, XidSeverityIgnored},
		{109, XidSeverityIgnored},

		// Critical (hardware failures)
		{48, XidSeverityCritical},
		{63, XidSeverityCritical},
		{64, XidSeverityCritical},
		{74, XidSeverityCritical},
		{79, XidSeverityCritical},
		{94, XidSeverityCritical},
		{95, XidSeverityCritical},
		{119, XidSeverityCritical},
		{120, XidSeverityCritical},

		// Warning (unknown XIDs)
		{1, XidSeverityWarning},
		{2, XidSeverityWarning},
		{999, XidSeverityWarning},
	}

	for _, tt := range tests {
		result := GetXidSeverity(tt.xid)
		if result != tt.expected {
			t.Errorf("GetXidSeverity(%d) = %v, want %v", tt.xid, result, tt.expected)
		}
	}
}

func TestXidSeverity_String(t *testing.T) {
	tests := []struct {
		severity XidSeverity
		expected string
	}{
		{XidSeverityUnknown, "unknown"},
		{XidSeverityIgnored, "ignored"},
		{XidSeverityWarning, "warning"},
		{XidSeverityCritical, "critical"},
		{XidSeverity(99), "unknown"}, // Invalid severity
	}

	for _, tt := range tests {
		result := tt.severity.String()
		if result != tt.expected {
			t.Errorf("XidSeverity(%d).String() = %q, want %q", tt.severity, result, tt.expected)
		}
	}
}
