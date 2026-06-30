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

package mcp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testAuthToken = "s3cret-token-9b3f"

// runAuthCase exercises requireBearerAuth with a single Authorization header
// and returns the recorded status plus whether the wrapped next handler was
// reached. Tests assert on both.
func runAuthCase(t *testing.T, authHeader string) (status int, reached bool) {
	t.Helper()

	h := &HTTPServer{authToken: testAuthToken}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true

		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}

	rr := httptest.NewRecorder()
	h.requireBearerAuth(next).ServeHTTP(rr, req)

	return rr.Code, reached
}

func TestRequireBearerAuth_MissingHeaderReturns401(t *testing.T) {
	status, reached := runAuthCase(t, "")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}

	if reached {
		t.Error("next handler must not be reached when Authorization is missing")
	}
}

func TestRequireBearerAuth_NonBearerSchemeReturns401(t *testing.T) {
	status, reached := runAuthCase(t, "Basic "+testAuthToken)
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}

	if reached {
		t.Error("next handler must not be reached for non-Bearer scheme")
	}
}

func TestRequireBearerAuth_WrongTokenReturns401(t *testing.T) {
	status, reached := runAuthCase(t, "Bearer wrong-token")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}

	if reached {
		t.Error("next handler must not be reached for wrong token")
	}
}

func TestRequireBearerAuth_EmptyBearerReturns401(t *testing.T) {
	status, reached := runAuthCase(t, "Bearer ")
	if status != http.StatusUnauthorized {
		t.Errorf("status: got %d, want 401", status)
	}

	if reached {
		t.Error("next handler must not be reached for empty Bearer token")
	}
}

func TestRequireBearerAuth_CorrectBearerReachesNext(t *testing.T) {
	status, reached := runAuthCase(t, "Bearer "+testAuthToken)
	if status != http.StatusOK {
		t.Errorf("status: got %d, want 200", status)
	}

	if !reached {
		t.Error("next handler must be reached when Bearer matches AuthToken")
	}
}
