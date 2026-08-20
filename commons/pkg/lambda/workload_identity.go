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

package lambda

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// IdentityLRNEnvVar names the service identity to assume, in the form
	// lrn:iam:identity:<id>. Injected by lambda-pod-identity-webhook from the
	// ServiceAccount's lambda.ai/identity-lrn annotation.
	IdentityLRNEnvVar = "LAMBDA_IDENTITY_LRN"

	// TokenFileEnvVar points at the projected ServiceAccount token the
	// webhook mounts. Also injected by the webhook.
	TokenFileEnvVar = "LAMBDA_WORKLOAD_IDENTITY_TOKEN_FILE" //nolint:gosec // path to a token, not a credential

	// DefaultTokenFile is where the webhook projects the token when
	// TokenFileEnvVar is unset.
	DefaultTokenFile = "/var/run/secrets/lambda.ai/serviceaccount/token" //nolint:gosec // path, not a credential

	// oidcTokenPath mints a short-lived API key from a ServiceAccount token.
	// It takes no Authorization header: the presented JWT is the credential.
	oidcTokenPath = "/api/v1/oidc/token" //nolint:gosec // URL path, not a credential

	// refreshWindow is how long before expiry a token is replaced, so a
	// request never rides one that is about to lapse.
	refreshWindow = 5 * time.Minute

	// jitterFrac spreads refreshes across a fleet so every pod does not
	// re-exchange at the same instant.
	jitterFrac = 0.2
)

// exchangeRequest is the body POST /api/v1/oidc/token takes.
type exchangeRequest struct {
	Token       string `json:"token"`
	IdentityLRN string `json:"identity_lrn"`
}

// exchangeResponse is the envelope it returns. Every failure is a 401 with no
// detail, by design: an unauthenticated caller must not learn why.
type exchangeResponse struct {
	Data struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		ExpiresAt   string `json:"expires_at"`
	} `json:"data"`
}

// cachedToken is a minted key and the two deadlines that govern it.
type cachedToken struct {
	accessToken string
	expiresAt   time.Time // the server's hard deadline
	refreshAt   time.Time // when we start minting a replacement
}

// workloadIdentity mints, caches and refreshes a short-lived API key from a
// projected ServiceAccount token. Safe for concurrent use.
type workloadIdentity struct {
	// exchangeClient carries noCredential, so minting a key never recurses
	// through the credential being minted.
	exchangeClient *Client
	identityLRN    string
	tokenFile      string
	clock          func() time.Time

	mu  sync.RWMutex // guards cur
	cur *cachedToken // nil until the first exchange

	refreshMu sync.Mutex // admits one refresher at a time
}

// newWorkloadIdentity builds the credential from the environment the webhook
// injected. It performs no I/O: the token file is read on first use, so a
// broken injection surfaces as a request error naming the path rather than at
// construction, where NewClient has no way to report it.
func newWorkloadIdentity(endpoint string, httpClient *http.Client, retry retryPolicy) *workloadIdentity {
	tokenFile := os.Getenv(TokenFileEnvVar)
	if tokenFile == "" {
		tokenFile = DefaultTokenFile
	}

	return &workloadIdentity{
		exchangeClient: &Client{
			endpoint: endpoint,
			http:     httpClient,
			retry:    retry,
			creds:    noCredential{},
		},
		identityLRN: os.Getenv(IdentityLRNEnvVar),
		tokenFile:   tokenFile,
		clock:       time.Now,
	}
}

// token returns the cached key, minting a replacement once it is due.
func (w *workloadIdentity) token(ctx context.Context) (string, error) {
	// Fast path: the cached key is not due for replacement yet.
	w.mu.RLock()
	cur := w.cur
	w.mu.RUnlock()

	if cur != nil && w.clock().Before(cur.refreshAt) {
		return cur.accessToken, nil
	}

	w.refreshMu.Lock()
	defer w.refreshMu.Unlock()

	// Another goroutine may have refreshed while we waited for the lock.
	w.mu.RLock()
	cur = w.cur
	w.mu.RUnlock()

	if cur != nil && w.clock().Before(cur.refreshAt) {
		return cur.accessToken, nil
	}

	fresh, err := w.exchange(ctx)
	if err != nil {
		// A key that has not actually expired outlives a failed refresh;
		// only a dead one surfaces the error.
		if cur != nil && w.clock().Before(cur.expiresAt) {
			return cur.accessToken, nil
		}

		return "", err
	}

	w.mu.Lock()
	w.cur = &fresh
	w.mu.Unlock()

	return fresh.accessToken, nil
}

// invalidate drops the cached key so the next request mints a fresh one. Called
// when the API rejects a request as unauthorized, which is how a key revoked
// before its stated expiry is noticed.
func (w *workloadIdentity) invalidate() {
	w.mu.Lock()
	w.cur = nil
	w.mu.Unlock()
}

// exchange trades the current ServiceAccount token for a new API key.
func (w *workloadIdentity) exchange(ctx context.Context) (cachedToken, error) {
	if w.identityLRN == "" {
		return cachedToken{}, fmt.Errorf("env var %s is not set", IdentityLRNEnvVar)
	}

	// Read the file every time: the kubelet rotates the projected token.
	raw, err := os.ReadFile(w.tokenFile)
	if err != nil {
		return cachedToken{}, fmt.Errorf("read workload identity token %s: %w", w.tokenFile, err)
	}

	saToken := strings.TrimSpace(string(raw))
	if saToken == "" {
		return cachedToken{}, fmt.Errorf("workload identity token %s is empty", w.tokenFile)
	}

	var parsed exchangeResponse

	req := exchangeRequest{Token: saToken, IdentityLRN: w.identityLRN}

	// Retries transient failures, unlike Post: minting a key has no side
	// effect worth protecting, and an unused key simply expires.
	payload, err := marshalJSON(req)
	if err != nil {
		return cachedToken{}, fmt.Errorf("exchange workload identity token: %w", err)
	}

	if err := w.exchangeClient.do(
		ctx, http.MethodPost, oidcTokenPath, nil, payload, &parsed, retryTransient,
	); err != nil {
		return cachedToken{}, fmt.Errorf("exchange workload identity token: %w", err)
	}

	if parsed.Data.AccessToken == "" {
		return cachedToken{}, fmt.Errorf("exchange workload identity token: response carried no access_token")
	}

	now := w.clock()
	expiresAt := expiryOf(parsed, now)
	replaceAt := refreshAt(expiresAt, now)

	slog.Info("Minted a Lambda API key from the workload identity",
		"identityLRN", w.identityLRN,
		"expiresAt", expiresAt.UTC().Format(time.RFC3339),
		"refreshAt", replaceAt.UTC().Format(time.RFC3339))

	return cachedToken{
		accessToken: parsed.Data.AccessToken,
		expiresAt:   expiresAt,
		refreshAt:   replaceAt,
	}, nil
}

// expiryOf prefers the absolute expires_at and falls back to expires_in. A
// response carrying neither is treated as already expired, so the next request
// mints again rather than reusing a key of unknown life.
func expiryOf(r exchangeResponse, now time.Time) time.Time {
	if r.Data.ExpiresAt != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, r.Data.ExpiresAt); err == nil {
				return t
			}
		}
	}

	if r.Data.ExpiresIn > 0 {
		return now.Add(time.Duration(r.Data.ExpiresIn) * time.Second)
	}

	return now
}

// refreshAt picks when to mint a replacement: refreshWindow before expiry,
// pulled earlier still by jitter, so refreshWindow is a floor on the margin
// rather than a ceiling. A key that lives less than the window aims for its
// half-life instead, so a short-lived one still gets replaced in time.
func refreshAt(expiresAt, now time.Time) time.Time {
	window := refreshWindow
	if life := expiresAt.Sub(now); window >= life {
		window = life / 2
	}

	jitter := time.Duration(randFrac() * jitterFrac * float64(window))

	at := expiresAt.Add(-(window + jitter))
	if !at.After(now) {
		return now.Add(expiresAt.Sub(now) / 2)
	}

	return at
}

// randFrac returns a random fraction in [0, 1) for refresh jitter only.
func randFrac() float64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0 // no randomness: skip jitter rather than fail the refresh
	}

	return float64(binary.BigEndian.Uint64(b[:])>>11) / float64(1<<53)
}
