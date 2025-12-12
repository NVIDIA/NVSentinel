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

// Package auditlogger implements an HTTP RoundTripper that transparently captures and logs write
// operations (POST, PUT, PATCH, DELETE) for audit purposes. The AuditingRoundTripper wraps any
// http.RoundTripper to intercept requests, execute them through the delegate, and log the operation
// details including method, URL, response code, and optionally the request body. This provides
// automatic audit logging for Kubernetes client-go and CSP SDK HTTP clients without modifying
// application code.
package auditlogger

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"

	"github.com/nvidia/nvsentinel/commons/pkg/envutil"
)

// AuditingRoundTripper is an HTTP RoundTripper that automatically captures
// and logs write operations (POST, PUT, PATCH, DELETE) to the audit log.
type AuditingRoundTripper struct {
	delegate       http.RoundTripper
	logRequestBody bool
}

// NewAuditingRoundTripper creates a new AuditingRoundTripper that wraps the given delegate.
// Use this to wrap client-go rest.Config.Wrap() or CSP SDK HTTP clients.
// The request body logging configuration is cached at creation time for performance.
func NewAuditingRoundTripper(delegate http.RoundTripper) *AuditingRoundTripper {
	return &AuditingRoundTripper{
		delegate:       delegate,
		logRequestBody: envutil.GetEnvBool(EnvAuditLogRequestBody, false),
	}
}

// RoundTrip implements http.RoundTripper interface.
// It logs write operations after executing the request to capture response codes.
func (rt *AuditingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string

	if isWriteMethod(req.Method) && rt.logRequestBody && req.Body != nil {
		bodyBytes, readErr := io.ReadAll(req.Body)
		// Always restore the body (even if partial) so the actual HTTP request can proceed
		req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		// Only capture for audit if read succeeded completely to avoid logging incomplete data
		if readErr == nil {
			body = string(bodyBytes)
		} else {
			slog.Error("audit: failed to read request body", "method", req.Method, "url", req.URL.String(), "error", readErr)
		}
	}

	resp, err := rt.delegate.RoundTrip(req)

	if isWriteMethod(req.Method) {
		responseCode := 0
		if resp != nil {
			responseCode = resp.StatusCode
		}

		Log(req.Method, req.URL.String(), body, responseCode)
	}

	return resp, err
}

// isWriteMethod returns true if the HTTP method is a write operation.
func isWriteMethod(method string) bool {
	return method == http.MethodPost ||
		method == http.MethodPut ||
		method == http.MethodPatch ||
		method == http.MethodDelete
}
