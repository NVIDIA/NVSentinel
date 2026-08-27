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
	"fmt"
	"log/slog"
	"net/http"
	"os"
)

// AuthMode names the credential a Client authenticates with.
type AuthMode string

const (
	// AuthWorkloadIdentity exchanges a projected Kubernetes ServiceAccount
	// token for a short-lived API key. No static secret on the cluster.
	AuthWorkloadIdentity AuthMode = "workload-identity"

	// AuthAPIKey sends the static key held in LAMBDA_API_KEY.
	AuthAPIKey AuthMode = "api-key"

	// AuthNone means neither credential is configured. Requests will fail.
	AuthNone AuthMode = "none"
)

// DetectAuthMode reports which credential the process environment implies.
//
// The pod-identity webhook injects the identity LRN into every pod whose
// ServiceAccount carries the lambda.ai/identity-lrn annotation, so its presence
// means the operator asked for workload identity and it wins over any static
// key that happens to also be set. A missing token file is then a broken
// injection, and surfaces as an error naming the path rather than as a silent
// fall back to the key.
//
// Callers use this to fail at startup and to log which credential is in play,
// rather than discovering it on the first request.
func DetectAuthMode() AuthMode {
	switch {
	case os.Getenv(IdentityLRNEnvVar) != "":
		return AuthWorkloadIdentity
	case os.Getenv(APIKeyEnvVar) != "":
		return AuthAPIKey
	default:
		return AuthNone
	}
}

// credentialSource supplies the bearer token for each request. The two
// implementations are the static key and workload identity; noCredential
// serves the unauthenticated token-exchange call itself.
type credentialSource interface {
	// token returns the bearer token to send, or "" to send no
	// Authorization header at all.
	token(ctx context.Context) (string, error)

	// invalidate drops any cached token so the next call mints a fresh one.
	// Called when the API rejects a request as unauthorized.
	invalidate()
}

// envAPIKey reads the static key from the environment on every request, so
// credential rotation works without a process restart.
type envAPIKey struct{}

func (envAPIKey) token(_ context.Context) (string, error) {
	key := os.Getenv(APIKeyEnvVar)
	if key == "" {
		return "", fmt.Errorf("env var %s is not set", APIKeyEnvVar)
	}

	return key, nil
}

// invalidate is a no-op: there is nothing cached, the env var is read every
// time.
func (envAPIKey) invalidate() {}

// noCredential sends no Authorization header. It backs the token-exchange
// call, which is unauthenticated and must not loop back through the
// credential it is minting.
type noCredential struct{}

func (noCredential) token(_ context.Context) (string, error) { return "", nil }

func (noCredential) invalidate() {}

// detectCredential builds the credential DetectAuthMode selects, and logs which
// one so the choice is visible from the client itself rather than inferred from
// the deployment. The unconfigured case is deferred rather than reported here:
// NewClient cannot fail, and envAPIKey already names the missing variable on
// first use.
func detectCredential(endpoint string, httpClient *http.Client, retry retryPolicy) credentialSource {
	mode := DetectAuthMode()

	slog.Info("Lambda API credential selected", "authMode", mode, "endpoint", endpoint)

	if mode == AuthWorkloadIdentity {
		// A static key left wired alongside an injected identity is ignored,
		// which is worth saying out loud: it usually means the deployment still
		// mounts a Secret it no longer needs.
		if os.Getenv(APIKeyEnvVar) != "" {
			slog.Warn("Both credentials are configured, using workload identity and ignoring the static key",
				"ignoredEnvVar", APIKeyEnvVar)
		}

		return newWorkloadIdentity(endpoint, httpClient, retry)
	}

	return envAPIKey{}
}
