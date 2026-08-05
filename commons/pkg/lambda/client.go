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

// Package lambda provides a shared HTTP client for the Lambda Cloud REST API.
// Used by csp-health-monitor and other Lambda-facing NVSentinel components.
package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// APIKeyEnvVar is the environment variable name for the Lambda API key.
	APIKeyEnvVar = "LAMBDA_API_KEY" //nolint:gosec // env var name, not the credential itself

	// DefaultTimeout is the default HTTP request timeout for a single attempt.
	DefaultTimeout = 30 * time.Second

	defaultMaxAttempts    = 4
	defaultInitialBackoff = 500 * time.Millisecond
	defaultBackoffFactor  = 2.0
	defaultBackoffJitter  = 0.2
)

// Client is an authenticated HTTP client for the Lambda Cloud API.
// The API key is read from LAMBDA_API_KEY on every request so credential
// rotation works without a process restart.
//
// Requests are retried with exponential backoff on transient failures
// (network errors, 5xx responses, and 429 Too Many Requests). Permanent
// failures (4xx other than 429, malformed responses, missing API key)
// short-circuit the retry loop.
type Client struct {
	endpoint string
	http     *http.Client
	retry    retryPolicy
}

type retryPolicy struct {
	maxAttempts    int
	initialBackoff time.Duration
	factor         float64
	jitter         float64
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient overrides the default *http.Client. Useful for tests that
// need to inject an httptest.NewServer's client.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithRetryPolicy overrides the retry defaults. maxAttempts includes the
// initial attempt, so maxAttempts=1 disables retries. Zero or negative
// values fall back to the built-in defaults.
func WithRetryPolicy(maxAttempts int, initialBackoff time.Duration, factor, jitter float64) Option {
	return func(c *Client) {
		if maxAttempts > 0 {
			c.retry.maxAttempts = maxAttempts
		}

		if initialBackoff > 0 {
			c.retry.initialBackoff = initialBackoff
		}

		if factor > 0 {
			c.retry.factor = factor
		}

		if jitter > 0 {
			c.retry.jitter = jitter
		}
	}
}

// NewClient constructs a Lambda API client. endpoint is the base URL,
// e.g. "https://cloud.lambda.ai".
func NewClient(endpoint string, opts ...Option) *Client {
	c := &Client{
		endpoint: endpoint,
		http:     &http.Client{Timeout: DefaultTimeout},
		retry: retryPolicy{
			maxAttempts:    defaultMaxAttempts,
			initialBackoff: defaultInitialBackoff,
			factor:         defaultBackoffFactor,
			jitter:         defaultBackoffJitter,
		},
	}

	for _, o := range opts {
		o(c)
	}

	return c
}

// Get performs an authenticated GET against endpoint+path with optional query
// params and decodes the JSON response body into out. If out is nil, the body
// is discarded. Retries transient failures with exponential backoff.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	apiKey := os.Getenv(APIKeyEnvVar)
	if apiKey == "" {
		return fmt.Errorf("env var %s is not set", APIKeyEnvVar)
	}

	u := c.endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	backoff := wait.Backoff{
		Steps:    c.retry.maxAttempts,
		Duration: c.retry.initialBackoff,
		Factor:   c.retry.factor,
		Jitter:   c.retry.jitter,
	}

	var (
		lastErr  error
		attempts int
	)

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		attempts++

		body, statusCode, doErr := c.doOnce(ctx, u, apiKey)
		if doErr != nil {
			// Transport-level failures (dial, TLS, i/o) are transient.
			lastErr = doErr
			slog.Debug("Lambda API request failed, will retry", "url", u, "error", doErr)

			return false, nil
		}

		if statusCode == http.StatusTooManyRequests || statusCode >= 500 {
			lastErr = fmt.Errorf("GET %s: status %d: %s", u, statusCode, body)
			slog.Debug("Lambda API returned retryable status", "url", u, "status", statusCode)

			return false, nil
		}

		if statusCode != http.StatusOK {
			// Permanent client error (401, 403, 404, ...) — do not retry.
			return false, fmt.Errorf("GET %s: status %d: %s", u, statusCode, body)
		}

		return true, decodeBody(body, out)
	})
	if err == nil {
		return nil
	}

	// Retry budget exhausted or context cancelled — surface the last observed error
	// and the actual number of attempts made (not the configured max, which would
	// misreport when we bailed early on a permanent error or cancelled context).
	if lastErr != nil {
		return fmt.Errorf("after %d attempts: %w", attempts, lastErr)
	}

	return err
}

// decodeBody unmarshals body into out when out is non-nil, otherwise discards
// body. Extracted from Get so the retry closure stays under the cyclomatic
// complexity limit.
func decodeBody(body []byte, out any) error {
	if out == nil {
		return nil
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}

	return nil
}

// doOnce performs a single request. It returns the response body and status
// code separately so the retry loop can classify the outcome without having
// to keep the *http.Response around.
func (c *Client) doOnce(ctx context.Context, u, apiKey string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s: %w", u, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	return body, resp.StatusCode, nil
}
