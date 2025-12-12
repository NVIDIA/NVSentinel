// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package auditlogger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInitAuditLogger(t *testing.T) {
	tests := []struct {
		name        string
		component   string
		setupEnv    func()
		cleanupEnv  func()
		expectError bool
	}{
		{
			name:      "successful initialization with valid component",
			component: "test-component",
			setupEnv: func() {
				os.Setenv(EnvAuditEnabled, "true")
				os.Setenv(EnvPodName, "test-pod")
				tmpDir := t.TempDir()
				os.Setenv(EnvAuditLogBasePath, tmpDir)
			},
			cleanupEnv: func() {
				os.Unsetenv(EnvAuditEnabled)
				os.Unsetenv(EnvPodName)
				os.Unsetenv(EnvAuditLogBasePath)
				CloseAuditLogger()
			},
			expectError: false,
		},
		{
			name:      "empty component name returns error",
			component: "",
			setupEnv: func() {
				tmpDir := t.TempDir()
				os.Setenv(EnvAuditLogBasePath, tmpDir)
			},
			cleanupEnv: func() {
				os.Unsetenv(EnvAuditLogBasePath)
			},
			expectError: true,
		},
		{
			name:      "audit disabled skips initialization",
			component: "disabled-component",
			setupEnv: func() {
				os.Setenv(EnvAuditEnabled, "false")
			},
			cleanupEnv: func() {
				os.Unsetenv(EnvAuditEnabled)
			},
			expectError: false,
		},
		{
			name:      "uses component name when POD_NAME not set",
			component: "fallback-component",
			setupEnv: func() {
				os.Setenv(EnvAuditEnabled, "true")
				tmpDir := t.TempDir()
				os.Setenv(EnvAuditLogBasePath, tmpDir)
				os.Unsetenv(EnvPodName)
			},
			cleanupEnv: func() {
				os.Unsetenv(EnvAuditEnabled)
				os.Unsetenv(EnvAuditLogBasePath)
				CloseAuditLogger()
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv()
			defer tt.cleanupEnv()

			err := InitAuditLogger(tt.component)
			if tt.expectError && err == nil {
				t.Error("InitAuditLogger() expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("InitAuditLogger() unexpected error: %v", err)
			}
		})
	}
}

func TestLog(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)

	err := InitAuditLogger("test-component")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}
	defer CloseAuditLogger()

	tests := []struct {
		name         string
		method       string
		url          string
		body         string
		responseCode int
		setupEnv     func()
		cleanupEnv   func()
		expectBody   bool
	}{
		{
			name:         "log POST without body logging",
			method:       "POST",
			url:          "https://example.com/api/v1/nodes",
			body:         `{"key":"value"}`,
			responseCode: 201,
			setupEnv: func() {
				os.Setenv(EnvAuditLogRequestBody, "false")
			},
			cleanupEnv: func() {
				os.Unsetenv(EnvAuditLogRequestBody)
			},
			expectBody: false,
		},
		{
			name:         "log PUT with body logging enabled",
			method:       "PUT",
			url:          "https://example.com/api/v1/nodes/test",
			body:         `{"spec":{"unschedulable":true}}`,
			responseCode: 200,
			setupEnv: func() {
				os.Setenv(EnvAuditLogRequestBody, "true")
			},
			cleanupEnv: func() {
				os.Unsetenv(EnvAuditLogRequestBody)
			},
			expectBody: true,
		},
		{
			name:         "log with 409 conflict response",
			method:       "PATCH",
			url:          "https://example.com/api/v1/nodes/test",
			body:         "",
			responseCode: 409,
			setupEnv:     func() {},
			cleanupEnv:   func() {},
			expectBody:   false,
		},
		{
			name:         "log DELETE with 200 OK",
			method:       "DELETE",
			url:          "https://example.com/api/v1/pods/test",
			body:         "",
			responseCode: 200,
			setupEnv:     func() {},
			cleanupEnv:   func() {},
			expectBody:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setupEnv()
			defer tt.cleanupEnv()

			Log(tt.method, tt.url, tt.body, tt.responseCode)

			logPath := filepath.Join(tmpDir, "test-pod-audit.log")
			content, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("Failed to read audit log: %v", err)
			}

			lines := strings.Split(strings.TrimSpace(string(content)), "\n")
			if len(lines) == 0 {
				t.Fatal("No audit log entries found")
			}

			lastLine := lines[len(lines)-1]
			var entry AuditEntry
			if err := json.Unmarshal([]byte(lastLine), &entry); err != nil {
				t.Fatalf("Failed to unmarshal audit entry: %v", err)
			}

			if entry.Component != "test-component" {
				t.Errorf("Component = %q, want %q", entry.Component, "test-component")
			}
			if entry.Method != tt.method {
				t.Errorf("Method = %q, want %q", entry.Method, tt.method)
			}
			if entry.URL != tt.url {
				t.Errorf("URL = %q, want %q", entry.URL, tt.url)
			}
			if entry.ResponseCode != tt.responseCode {
				t.Errorf("ResponseCode = %d, want %d", entry.ResponseCode, tt.responseCode)
			}
			if tt.expectBody && entry.RequestBody != tt.body {
				t.Errorf("RequestBody = %q, want %q", entry.RequestBody, tt.body)
			}
			if !tt.expectBody && entry.RequestBody != "" {
				t.Errorf("RequestBody should be empty but got %q", entry.RequestBody)
			}
		})
	}
}

// TestLogWhenNotInitialized verifies that calling Log() without prior initialization
// does not panic and is a silent no-op.
func TestLogWhenNotInitialized(t *testing.T) {
	auditLogger = nil

	assert.NotPanics(t, func() {
		Log("POST", "https://example.com/test", "body", 200)
	}, "Log() should not panic when called without initialization")

	if auditLogger != nil {
		t.Error("auditLogger should remain nil when Log() is called without initialization")
	}
}

func TestCloseAuditLogger(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)

	err := InitAuditLogger("test-component")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}

	err = CloseAuditLogger()
	if err != nil {
		t.Errorf("CloseAuditLogger() returned error: %v", err)
	}

	err = CloseAuditLogger()
	if err != nil {
		t.Errorf("CloseAuditLogger() on nil logger returned error: %v", err)
	}
}
