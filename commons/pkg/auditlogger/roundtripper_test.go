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
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type mockRoundTripper struct {
	response *http.Response
	err      error
}

func (m *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.response, m.err
}

func TestIsWriteMethod(t *testing.T) {
	tests := []struct {
		method   string
		expected bool
	}{
		{"POST", true},
		{"PUT", true},
		{"PATCH", true},
		{"DELETE", true},
		{"GET", false},
		{"HEAD", false},
		{"OPTIONS", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			result := isWriteMethod(tt.method)
			if result != tt.expected {
				t.Errorf("isWriteMethod(%q) = %t, want %t", tt.method, result, tt.expected)
			}
		})
	}
}

func TestNewAuditingRoundTripper(t *testing.T) {
	delegate := &mockRoundTripper{}
	rt := NewAuditingRoundTripper(delegate)

	if rt == nil {
		t.Fatal("NewAuditingRoundTripper returned nil")
	}

	if rt.delegate != delegate {
		t.Error("NewAuditingRoundTripper did not set delegate correctly")
	}
}

func TestAuditingRoundTripper_WriteMethod(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)

	err := InitAuditLogger("roundtripper-test")
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
		logBody      bool
	}{
		{
			name:         "POST with 201 Created",
			method:       "POST",
			url:          "https://example.com/api/v1/nodes",
			body:         `{"name":"test-node"}`,
			responseCode: 201,
			logBody:      false,
		},
		{
			name:         "PUT with 200 OK",
			method:       "PUT",
			url:          "https://example.com/api/v1/nodes/test-node",
			body:         `{"spec":{"unschedulable":true}}`,
			responseCode: 200,
			logBody:      true,
		},
		{
			name:         "PATCH with 200 OK",
			method:       "PATCH",
			url:          "https://example.com/api/v1/nodes/test-node",
			body:         `[{"op":"add","path":"/metadata/labels/key","value":"value"}]`,
			responseCode: 200,
			logBody:      false,
		},
		{
			name:         "DELETE with 200 OK",
			method:       "DELETE",
			url:          "https://example.com/api/v1/pods/test-pod",
			body:         "",
			responseCode: 200,
			logBody:      false,
		},
		{
			name:         "POST with 409 Conflict",
			method:       "POST",
			url:          "https://example.com/api/v1/configmaps",
			body:         `{"metadata":{"name":"test"}}`,
			responseCode: 409,
			logBody:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.logBody {
				os.Setenv(EnvAuditLogRequestBody, "true")
				defer os.Unsetenv(EnvAuditLogRequestBody)
			}

			mockResp := &http.Response{
				StatusCode: tt.responseCode,
				Body:       io.NopCloser(bytes.NewBufferString("response body")),
			}
			delegate := &mockRoundTripper{response: mockResp, err: nil}
			rt := NewAuditingRoundTripper(delegate)

			var reqBody io.ReadCloser
			if tt.body != "" {
				reqBody = io.NopCloser(bytes.NewBufferString(tt.body))
			}

			req, _ := http.NewRequest(tt.method, tt.url, reqBody)
			resp, err := rt.RoundTrip(req)

			if err != nil {
				t.Fatalf("RoundTrip() error = %v", err)
			}
			if resp.StatusCode != tt.responseCode {
				t.Errorf("StatusCode = %d, want %d", resp.StatusCode, tt.responseCode)
			}

			logPath := filepath.Join(tmpDir, "test-pod-audit.log")
			content, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("Failed to read audit log: %v", err)
			}

			lines := strings.Split(strings.TrimSpace(string(content)), "\n")
			lastLine := lines[len(lines)-1]

			var entry AuditEntry
			if err := json.Unmarshal([]byte(lastLine), &entry); err != nil {
				t.Fatalf("Failed to unmarshal audit entry: %v", err)
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
			if tt.logBody && entry.RequestBody != tt.body {
				t.Errorf("RequestBody = %q, want %q", entry.RequestBody, tt.body)
			}
		})
	}
}

func TestAuditingRoundTripper_ReadMethod(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod-read")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)

	err := InitAuditLogger("roundtripper-read-test")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}
	defer CloseAuditLogger()

	mockResp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString("response body")),
	}
	delegate := &mockRoundTripper{response: mockResp, err: nil}
	rt := NewAuditingRoundTripper(delegate)

	req, _ := http.NewRequest("GET", "https://example.com/api/v1/nodes", nil)
	_, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}

	logPath := filepath.Join(tmpDir, "test-pod-read-audit.log")
	content, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("Failed to read audit log: %v", err)
	}

	if strings.Contains(string(content), "GET") {
		t.Error("GET request should not be logged in audit log")
	}
}

func TestAuditingRoundTripper_ErrorResponse(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod-error")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)

	err := InitAuditLogger("roundtripper-error-test")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}
	defer CloseAuditLogger()

	expectedErr := errors.New("network error")
	delegate := &mockRoundTripper{response: nil, err: expectedErr}
	rt := NewAuditingRoundTripper(delegate)

	req, _ := http.NewRequest("POST", "https://example.com/api/v1/nodes", nil)
	resp, err := rt.RoundTrip(req)

	if err != expectedErr {
		t.Errorf("Expected error %v, got %v", expectedErr, err)
	}
	if resp != nil {
		t.Error("Response should be nil when error occurs")
	}

	logPath := filepath.Join(tmpDir, "test-pod-error-audit.log")
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read audit log: %v", err)
	}

	var entry AuditEntry
	if err := json.Unmarshal(content, &entry); err != nil {
		t.Fatalf("Failed to unmarshal audit entry: %v", err)
	}

	if entry.ResponseCode != 0 {
		t.Errorf("ResponseCode should be 0 for error case, got %d", entry.ResponseCode)
	}
}

func TestAuditingRoundTripper_NilRequestBody(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod-nilbody")
	os.Setenv(EnvAuditLogRequestBody, "true")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)
	defer os.Unsetenv(EnvAuditLogRequestBody)

	err := InitAuditLogger("roundtripper-nilbody-test")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}
	defer CloseAuditLogger()

	mockResp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString("response")),
	}
	delegate := &mockRoundTripper{response: mockResp, err: nil}
	rt := NewAuditingRoundTripper(delegate)

	req, _ := http.NewRequest("POST", "https://example.com/api/v1/test", nil)
	_, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}

	logPath := filepath.Join(tmpDir, "test-pod-nilbody-audit.log")
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read audit log: %v", err)
	}

	var entry AuditEntry
	if err := json.Unmarshal(content, &entry); err != nil {
		t.Fatalf("Failed to unmarshal audit entry: %v", err)
	}

	if entry.RequestBody != "" {
		t.Errorf("RequestBody should be empty for nil body, got %q", entry.RequestBody)
	}
}

type errorReader struct{}

func (e *errorReader) Read(p []byte) (n int, err error) {
	return 0, errors.New("read error")
}

func TestAuditingRoundTripper_ReadAllError(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod-readerror")
	os.Setenv(EnvAuditLogRequestBody, "true")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)
	defer os.Unsetenv(EnvAuditLogRequestBody)

	err := InitAuditLogger("roundtripper-readerror-test")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}
	defer CloseAuditLogger()

	mockResp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString("response")),
	}
	delegate := &mockRoundTripper{response: mockResp, err: nil}
	rt := NewAuditingRoundTripper(delegate)

	req, _ := http.NewRequest("POST", "https://example.com/api/v1/test", io.NopCloser(&errorReader{}))
	resp, err := rt.RoundTrip(req)

	if err != nil {
		t.Fatalf("RoundTrip() should not fail even with read error: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", resp.StatusCode)
	}

	logPath := filepath.Join(tmpDir, "test-pod-readerror-audit.log")
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("Failed to read audit log: %v", err)
	}

	var entry AuditEntry
	if err := json.Unmarshal(content, &entry); err != nil {
		t.Fatalf("Failed to unmarshal audit entry: %v", err)
	}

	if entry.RequestBody != "" {
		t.Errorf("RequestBody should be empty when read fails, got %q", entry.RequestBody)
	}
	if entry.ResponseCode != 200 {
		t.Errorf("ResponseCode = %d, want 200", entry.ResponseCode)
	}
}

type bodyCapturingRoundTripper struct {
	bodyRead      string
	bodyReadCount int
	response      *http.Response
	t             *testing.T
}

func (b *bodyCapturingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		bodyBytes, err := io.ReadAll(req.Body)
		if err != nil {
			b.t.Fatalf("Delegate failed to read body: %v", err)
		}
		b.bodyRead = string(bodyBytes)
		b.bodyReadCount++
	}
	return b.response, nil
}

func TestAuditingRoundTripper_PreservesRequestBody(t *testing.T) {
	tmpDir := t.TempDir()
	os.Setenv(EnvAuditEnabled, "true")
	os.Setenv(EnvAuditLogBasePath, tmpDir)
	os.Setenv(EnvPodName, "test-pod-preserve")
	os.Setenv(EnvAuditLogRequestBody, "true")
	defer os.Unsetenv(EnvAuditEnabled)
	defer os.Unsetenv(EnvAuditLogBasePath)
	defer os.Unsetenv(EnvPodName)
	defer os.Unsetenv(EnvAuditLogRequestBody)

	err := InitAuditLogger("roundtripper-preserve-test")
	if err != nil {
		t.Fatalf("InitAuditLogger() failed: %v", err)
	}
	defer CloseAuditLogger()

	originalBody := `{"test":"data"}`
	mockResp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewBufferString("response")),
	}

	delegate := &bodyCapturingRoundTripper{
		response: mockResp,
		t:        t,
	}

	rt := NewAuditingRoundTripper(delegate)
	req, _ := http.NewRequest("POST", "https://example.com/api/v1/test", io.NopCloser(bytes.NewBufferString(originalBody)))
	_, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}

	if delegate.bodyReadCount != 1 {
		t.Errorf("Delegate read body %d times, want 1", delegate.bodyReadCount)
	}
	if delegate.bodyRead != originalBody {
		t.Errorf("Body in delegate = %q, want %q", delegate.bodyRead, originalBody)
	}
}
