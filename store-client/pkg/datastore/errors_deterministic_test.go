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

package datastore

import (
	"errors"
	"testing"

	"github.com/lib/pq"
)

func TestIsDeterministicError(t *testing.T) {
	for name, test := range map[string]struct {
		err  error
		want bool
	}{
		"validation": {
			err:  NewValidationError(ProviderPostgreSQL, "bad pipeline", nil),
			want: true,
		},
		"postgres data exception": {
			err:  NewQueryError(ProviderPostgreSQL, "bad row", &pq.Error{Code: "22P02"}),
			want: true,
		},
		"postgres syntax error": {
			err:  NewQueryError(ProviderPostgreSQL, "bad SQL", &pq.Error{Code: "42601"}),
			want: true,
		},
		"postgres undefined table": {
			err: NewQueryError(ProviderPostgreSQL, "table not provisioned yet", &pq.Error{Code: "42P01"}),
		},
		"postgres permission error": {
			err: NewQueryError(ProviderPostgreSQL, "permission", &pq.Error{Code: "42501"}),
		},
		"unknown query failure": {
			err: NewQueryError(ProviderMongoDB, "query failed", errors.New("server unavailable")),
		},
		"connection failure": {
			err: NewConnectionError(ProviderPostgreSQL, "offline", errors.New("refused")),
		},
		"plain error": {err: errors.New("plain")},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsDeterministicError(test.err); got != test.want {
				t.Fatalf("IsDeterministicError() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestDeterministicPostgreSQLQuerySQLStates(t *testing.T) {
	for _, code := range []string{"22000", "42601", "42703", "42804", "42883"} {
		t.Run(code, func(t *testing.T) {
			err := NewQueryError(ProviderPostgreSQL, "invalid deterministic query", &pq.Error{Code: pq.ErrorCode(code)})
			if !IsDeterministicError(err) {
				t.Fatalf("SQLSTATE %s classified as transient", code)
			}
		})
	}
}
