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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
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

	// maxRedirects mirrors net/http's own cap, which is dropped the moment
	// CheckRedirect is set.
	maxRedirects = 10
)

// errRedirect marks a redirect this client refused to follow. A redirect chain
// is deterministic, so retrying only replays it.
var errRedirect = errors.New("redirect refused")

// Client is an authenticated HTTP client for the Lambda Cloud API.
// The API key is read from LAMBDA_API_KEY on every request so credential
// rotation works without a process restart.
//
// Requests are retried with exponential backoff on transient failures
// (network errors, 5xx responses, and 429 Too Many Requests). Permanent
// failures (4xx other than 429, malformed responses, missing API key)
// short-circuit the retry loop. Post retries on less, see retryRateLimitOnly.
type Client struct {
	endpoint    string
	workspaceID string
	http        *http.Client
	retry       retryPolicy
}

// retryPolicy is the exponential-backoff schedule applied to a request.
// maxAttempts counts the initial attempt.
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

// WithWorkspaceID scopes requests to a Lambda workspace. An empty ID leaves the
// parameter off, so the API defaults to the key's own workspace.
//
// Only ListMaintenanceEvents takes it today, the instance endpoints resolve the
// workspace from the instance itself.
func WithWorkspaceID(workspaceID string) Option {
	return func(c *Client) { c.workspaceID = workspaceID }
}

// workspaceIDPattern matches a UUID with or without dashes, the two forms the
// API accepts for workspace_id.
var workspaceIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`,
)

// ValidWorkspaceID reports whether id is in a form the API accepts for
// workspace_id. Callers check it at startup: the API answers 400 to anything
// else, which would otherwise repeat on every request.
func ValidWorkspaceID(id string) bool {
	return workspaceIDPattern.MatchString(id)
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

	// Wrapped rather than assigned outright, and after the options so an
	// injected client cannot opt out of the scheme check.
	c.http.CheckRedirect = refuseInsecureRedirect(c.http.CheckRedirect)

	return c
}

// redirectPolicy is net/http's CheckRedirect signature.
type redirectPolicy func(req *http.Request, via []*http.Request) error

// refuseInsecureRedirect wraps next with a scheme check. net/http copies
// Authorization to the same host or a subdomain without looking at the scheme,
// so a downgrade would hand the API key to a plaintext listener.
//
// Setting CheckRedirect at all replaces net/http's ten-redirect cap, so with no
// next to defer to this reimposes it. Without that an https to https loop runs
// until the client timeout.
func refuseInsecureRedirect(next redirectPolicy) redirectPolicy {
	return func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("%w: %s would send the API key over %s",
				errRedirect, req.URL.Redacted(), req.URL.Scheme)
		}

		if next != nil {
			return next(req, via)
		}

		if len(via) >= maxRedirects {
			return fmt.Errorf("%w: stopped after %d redirects", errRedirect, maxRedirects)
		}

		return nil
	}
}

// Get performs an authenticated GET against endpoint+path with optional query
// params and decodes the JSON response body into out. If out is nil, the body
// is discarded. Retries transient failures with exponential backoff.
func (c *Client) Get(ctx context.Context, path string, query url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, query, nil, out, retryTransient)
}

// Post performs an authenticated POST against endpoint+path with in marshalled
// as the JSON request body, and decodes the JSON response into out.
//
// It serves the instance-operations endpoints, which are not idempotent and
// take no idempotency key, so it retries on less than Get does. See
// retryRateLimitOnly.
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	// Marshalled once so every retry resends identical bytes.
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	return c.do(ctx, http.MethodPost, path, nil, payload, out, retryRateLimitOnly)
}

// do performs an authenticated request and decodes the JSON response into out.
// payload is nil for methods without a request body.
func (c *Client) do(
	ctx context.Context, method, path string, query url.Values, payload []byte, out any, retry retryScope,
) error {
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

		body, statusCode, doErr := c.doOnce(ctx, method, u, apiKey, payload)
		if doErr != nil {
			// Transport-level failures (dial, TLS, i/o).
			if !retry(statusCode, doErr) {
				return false, doErr
			}

			lastErr = doErr
			slog.Debug("Lambda API request failed, will retry", "url", u, "error", doErr)

			return false, nil
		}

		if statusCode != http.StatusOK {
			statusErr := fmt.Errorf("%s %s: status %d: %s", method, u, statusCode, body)
			if !retry(statusCode, nil) {
				return false, statusErr
			}

			lastErr = statusErr

			slog.Debug("Lambda API returned retryable status", "url", u, "status", statusCode)

			return false, nil
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

// retryScope reports whether a failed attempt can be repeated. transportErr is
// nil when the request completed, in which case statusCode holds the response
// status.
type retryScope func(statusCode int, transportErr error) bool

// retryTransient retries network errors, 429, and 5xx. Safe for a request that
// can be repeated without side effects. A refused redirect is excluded: the
// chain is deterministic, so a repeat earns the same refusal.
func retryTransient(statusCode int, transportErr error) bool {
	if errors.Is(transportErr, errRedirect) {
		return false
	}

	return transportErr != nil || statusCode == http.StatusTooManyRequests || statusCode >= 500
}

// retryRateLimitOnly retries only 429, where the rate limiter rejected the
// request without acting on it. A transport error or 5xx is ambiguous, the
// request may already have taken effect so it surfaces on the first attempt
// rather than resubmitting an operation that cannot be undone.
func retryRateLimitOnly(statusCode int, transportErr error) bool {
	return transportErr == nil && statusCode == http.StatusTooManyRequests
}

// decodeBody unmarshals body into out when out is non-nil, otherwise discards
// body. Extracted from do so the retry closure stays under the cyclomatic
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
func (c *Client) doOnce(ctx context.Context, method, u, apiKey string, payload []byte) ([]byte, int, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}

	return body, resp.StatusCode, nil
}
