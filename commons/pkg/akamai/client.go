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

// Package akamai provides a shared HTTP client for the Akamai Cloud (Linode)
// REST API. Used by csp-health-monitor and janitor-provider.
package akamai

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
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// APITokenEnvVar is the environment variable name for the Akamai Cloud API
	// token. It is the variable the Linode CLI and SDKs read, so the same
	// Secret works for both.
	APITokenEnvVar = "LINODE_TOKEN" //nolint:gosec // env var name, not the credential itself

	// DefaultTimeout is the default HTTP request timeout for a single attempt.
	DefaultTimeout = 30 * time.Second

	// pageSize is the largest page the API serves, which keeps list calls to
	// as few requests as it allows.
	pageSize = 500

	// maxPages caps the pagination loop so a misbehaving API cannot hang the
	// caller or grow memory without bound.
	maxPages = 1000

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

// Client is an authenticated HTTP client for the Akamai Cloud API.
//
// It authenticates with the static token in LINODE_TOKEN. The token is read on
// every request so rotating the Secret it comes from works without a process
// restart.
//
// Requests are retried with exponential backoff on transient failures
// (network errors, 5xx responses, and 429 Too Many Requests). Permanent
// failures (4xx other than 429, malformed responses, missing credential)
// short-circuit the retry loop. Post and Delete retry on less, see
// retryRateLimitOnly.
type Client struct {
	endpoint string
	token    func() string
	http     *http.Client
	retry    retryPolicy
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

// NewClient constructs an Akamai Cloud API client. endpoint is the base URL
// including the API version, e.g. "https://api.linode.com/v4", and must have
// passed NormalizeEndpoint.
func NewClient(endpoint string, opts ...Option) *Client {
	c := &Client{
		endpoint: endpoint,
		token:    func() string { return os.Getenv(APITokenEnvVar) },
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
	// The injected client is copied first so its owner's redirect policy is left
	// as it was.
	hc := *c.http
	hc.CheckRedirect = refuseInsecureRedirect(hc.CheckRedirect)
	c.http = &hc

	return c
}

// redirectPolicy is net/http's CheckRedirect signature.
type redirectPolicy func(req *http.Request, via []*http.Request) error

// refuseInsecureRedirect wraps next with a scheme check. net/http copies
// Authorization to the same host or a subdomain without looking at the scheme,
// so a downgrade would hand the API token to a plaintext listener.
//
// Setting CheckRedirect at all replaces net/http's ten-redirect cap, so with no
// next to defer to this reimposes it. Without that an https to https loop runs
// until the client timeout.
func refuseInsecureRedirect(next redirectPolicy) redirectPolicy {
	return func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("%w: %s would send the API token over %s",
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
// It serves the instance and node operations, which are not idempotent and take
// no idempotency key, so it retries on less than Get does. See
// retryRateLimitOnly.
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	payload, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("marshal request body: %w", err)
	}

	return c.do(ctx, http.MethodPost, path, nil, payload, out, retryRateLimitOnly)
}

// Delete performs an authenticated DELETE against endpoint+path. Like Post it
// is not safe to repeat, so it retries only on 429.
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil, out, retryRateLimitOnly)
}

// do performs an authenticated request and decodes the JSON response into out.
// payload is nil for methods without a request body.
func (c *Client) do(
	ctx context.Context, method, path string, query url.Values, payload []byte, out any, retry retryScope,
) error {
	token := c.token()
	if token == "" {
		return fmt.Errorf("no Akamai API token: set %s", APITokenEnvVar)
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

	var run attemptLog

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		return run.attempt(ctx, c, request{method: method, url: u, token: token, payload: payload}, out, retry)
	})

	return run.result(err)
}

// request is one HTTP request the retry loop sends.
type request struct {
	method, url, token string
	payload            []byte
}

// attemptLog records what the retry loop needs once it ends: the error of the
// most recent attempt, retried or not, how many attempts ran, and whether the
// last one failed for good.
type attemptLog struct {
	lastErr   error
	count     int
	permanent bool
}

// attempt sends the request once and reports whether the loop is done. An error
// that is not retried ends the loop and is recorded as permanent.
func (l *attemptLog) attempt(
	ctx context.Context, c *Client, r request, out any, retry retryScope,
) (bool, error) {
	l.count++

	body, statusCode, doErr := c.doOnce(ctx, r.method, r.url, r.token, r.payload)
	if doErr != nil {
		// Transport-level failures (dial, TLS, i/o).
		return false, l.fail(doErr, retry(statusCode, doErr), "Akamai API request failed, will retry", r.url)
	}

	if statusCode != http.StatusOK {
		statusErr := &StatusError{Method: r.method, URL: r.url, StatusCode: statusCode, Body: string(body)}

		return false, l.fail(statusErr, retry(statusCode, nil), "Akamai API returned retryable status", r.url)
	}

	decodeErr := decodeBody(body, out)
	if decodeErr != nil {
		l.lastErr, l.permanent = decodeErr, true
	}

	return true, decodeErr
}

// fail records err as the error of the latest attempt. It returns nil when the
// attempt can be repeated, so the loop goes on, and err otherwise.
func (l *attemptLog) fail(err error, retryable bool, retryMsg, url string) error {
	l.lastErr = err

	if !retryable {
		l.permanent = true

		return err
	}

	slog.Debug(retryMsg, "url", url, "error", err)

	return nil
}

// result turns the end of the loop into the error to return. A first attempt
// that failed for good returns its own error. Otherwise the retry budget ran
// out, the context ended, or a retried request then failed for good, so it
// surfaces the error of the last attempt and the actual number of attempts made
// (not the configured max, which would misreport when we bailed early on a
// permanent error or cancelled context). The last attempt is the one recorded
// in lastErr, never an older retried error.
func (l *attemptLog) result(err error) error {
	if err == nil {
		return nil
	}

	if l.lastErr == nil || (l.permanent && l.count == 1) {
		return err
	}

	return fmt.Errorf("after %d attempts: %w", l.count, l.lastErr)
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
func (c *Client) doOnce(ctx context.Context, method, u, token string, payload []byte) ([]byte, int, error) {
	var reqBody io.Reader
	if payload != nil {
		reqBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

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

// StatusError is returned when the API answers with a status other than 200.
// Callers that need to tell a refusal that will not clear, such as a rejected
// token or a missing instance, from a failure that may pass can test StatusCode
// with errors.As.
type StatusError struct {
	Method     string
	URL        string
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.URL, e.StatusCode, e.Body)
}

// page is the envelope every paginated list endpoint returns.
type page[T any] struct {
	Data  []T `json:"data"`
	Page  int `json:"page"`
	Pages int `json:"pages"`
	// Results is the total item count across all pages. It is a pointer so an
	// absent field is distinguishable from zero.
	Results *int `json:"results"`
}

// listAll walks a paginated list endpoint and returns every item. Retry/backoff
// is handled by the underlying Client. When the last page reports a total item
// count ("results"), listAll checks it against the number of items collected
// and returns an error on mismatch, so a short or truncated list is never
// mistaken for a complete one.
func listAll[T any](ctx context.Context, c *Client, path string) ([]T, error) {
	var all []T

	for p := 1; p <= maxPages; p++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(p))
		q.Set("page_size", strconv.Itoa(pageSize))

		var parsed page[T]
		if err := c.Get(ctx, path, q, &parsed); err != nil {
			return nil, err
		}

		all = append(all, parsed.Data...)

		if p >= parsed.Pages {
			if parsed.Results != nil && *parsed.Results != len(all) {
				return nil, fmt.Errorf("%s returned %d items but the API reports %d", path, len(all), *parsed.Results)
			}

			return all, nil
		}
	}

	return nil, fmt.Errorf("%s pagination exceeded %d pages", path, maxPages)
}
