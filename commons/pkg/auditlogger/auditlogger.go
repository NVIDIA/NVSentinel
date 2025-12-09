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

// Package auditlogger provides file-based audit logging for write operations in NVSentinel components.
// It automatically captures and logs HTTP write operations (POST, PUT, PATCH, DELETE) with timestamps,
// component identification, request details, and response codes. The package supports automatic log
// rotation using lumberjack and provides configurable options through environment variables for log
// directory, file size limits, retention policies, and optional request body logging.
package auditlogger

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/envutil"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	DefaultAuditLogDir     = "/var/log/nvsentinel"
	EnvAuditLogBasePath    = "AUDIT_LOG_BASE_PATH"
	EnvAuditEnabled        = "AUDIT_ENABLED"
	EnvAuditLogRequestBody = "AUDIT_LOG_REQUEST_BODY"
	EnvPodName             = "POD_NAME"
	EnvAuditLogMaxSize     = "AUDIT_LOG_MAX_SIZE_MB"
	EnvAuditLogMaxBackups  = "AUDIT_LOG_MAX_BACKUPS"
	EnvAuditLogMaxAge      = "AUDIT_LOG_MAX_AGE_DAYS"
	EnvAuditLogCompress    = "AUDIT_LOG_COMPRESS"
)

// AuditEntry represents a single audit log entry for an HTTP write operation, capturing the
// Timestamp (RFC3339 format), Component name, HTTP Method and URL, ResponseCode from the server,
// and optionally the RequestBody if configured via AUDIT_LOG_REQUEST_BODY environment variable.
type AuditEntry struct {
	Timestamp    string `json:"timestamp"`
	Component    string `json:"component"`
	Method       string `json:"method"`
	URL          string `json:"url"`
	ResponseCode int    `json:"response_code,omitempty"`
	RequestBody  string `json:"requestBody,omitempty"`
}

var (
	auditLogger *lumberjack.Logger
	component   string
)

// InitAuditLogger initializes the audit logger with the given component name.
// It creates a log file at {AUDIT_LOG_BASE_PATH}/{POD_NAME}-audit.log with automatic rotation.
// Returns an error if initialization fails.
func InitAuditLogger(comp string) error {
	if comp == "" {
		return fmt.Errorf("component name cannot be empty")
	}

	if !envutil.GetEnvBool(EnvAuditEnabled, true) {
		return nil
	}

	component = comp

	baseDir := os.Getenv(EnvAuditLogBasePath)
	if baseDir == "" {
		baseDir = DefaultAuditLogDir
	}

	podName := os.Getenv(EnvPodName)
	if podName == "" {
		podName = component
	}

	logPath := filepath.Join(baseDir, podName+"-audit.log")

	auditLogger = &lumberjack.Logger{
		Filename:   logPath,
		MaxSize:    envutil.GetEnvInt(EnvAuditLogMaxSize, 100),
		MaxBackups: envutil.GetEnvInt(EnvAuditLogMaxBackups, 7),
		MaxAge:     envutil.GetEnvInt(EnvAuditLogMaxAge, 30),
		Compress:   envutil.GetEnvBool(EnvAuditLogCompress, true),
	}

	return nil
}

// CloseAuditLogger closes the audit logger and flushes any remaining logs.
// Should be called with defer in main() for graceful shutdown.
func CloseAuditLogger() error {
	if auditLogger != nil {
		return auditLogger.Close()
	}

	return nil
}

// Log writes an audit entry to the audit log file.
// The request body is only included if AUDIT_LOG_REQUEST_BODY=true.
// The responseCode should be the HTTP status code; use 0 if not available.
func Log(method, url, body string, responseCode int) {
	if auditLogger == nil {
		return
	}

	entry := AuditEntry{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Component:    component,
		Method:       method,
		URL:          url,
		ResponseCode: responseCode,
	}

	if envutil.GetEnvBool(EnvAuditLogRequestBody, false) && body != "" {
		entry.RequestBody = body
	}

	if err := json.NewEncoder(auditLogger).Encode(entry); err != nil {
		slog.Error("audit: failed to write entry", "error", err)
	}
}
