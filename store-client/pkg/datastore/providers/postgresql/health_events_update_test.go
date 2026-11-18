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

package postgresql

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestStatusDecisionsPatch(t *testing.T) {
	drain := &datastore.DrainDecision{
		RuleSet: "slurm",
		Method:  "Custom",
		Stopped: true,
		Scope:   &datastore.Entity{EntityType: "GPU", EntityValue: "0"},
	}
	remediation := &datastore.RemediationDecision{RuleSet: "vm-replace", Action: "REPLACE_VM"}

	tests := []struct {
		name   string
		status datastore.HealthEventStatus
		want   string
	}{
		{
			name:   "no decision",
			status: datastore.HealthEventStatus{},
			want:   `{}`,
		},
		{
			name:   "drain decision only",
			status: datastore.HealthEventStatus{DrainDecision: drain},
			want:   `{"draindecision":{"method":"Custom","ruleset":"slurm","scope":{"entitytype":"GPU","entityvalue":"0"},"stopped":true}}`,
		},
		{
			name: "both decisions",
			status: datastore.HealthEventStatus{
				DrainDecision:       drain,
				RemediationDecision: remediation,
			},
			want: `{"draindecision":{"method":"Custom","ruleset":"slurm","scope":{"entitytype":"GPU","entityvalue":"0"},"stopped":true},"remediationdecision":{"action":"REPLACE_VM","ruleset":"vm-replace"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := statusDecisionsPatch(tt.status)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, got)
		})
	}
}

func TestUpdateHealthEventStatus_MergesDecisionsPatch(t *testing.T) {
	quarantined := datastore.Quarantined
	remediated := true
	drain := &datastore.DrainDecision{RuleSet: "slurm", Method: "Custom"}
	remediation := &datastore.RemediationDecision{Action: "REPLACE_VM"}

	tests := []struct {
		name     string
		status   datastore.HealthEventStatus
		query    string
		argCount int
	}{
		{
			name: "quarantine update",
			status: datastore.HealthEventStatus{
				NodeQuarantined:     &quarantined,
				DrainDecision:       drain,
				RemediationDecision: remediation,
			},
			query:    `\|\| \$10::jsonb`,
			argCount: 10,
		},
		{
			name: "drain update",
			status: datastore.HealthEventStatus{
				UserPodsEvictionStatus: datastore.OperationStatus{Status: datastore.StatusSucceeded},
				DrainDecision:          drain,
			},
			query:    `\|\| \$9::jsonb`,
			argCount: 9,
		},
		{
			name:     "remediation update without a decision",
			status:   datastore.HealthEventStatus{FaultRemediated: &remediated},
			query:    `\|\| \$5::jsonb`,
			argCount: 5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)

			patch, err := statusDecisionsPatch(tt.status)
			require.NoError(t, err)

			args := make([]driver.Value, tt.argCount)
			for i := range args {
				args[i] = sqlmock.AnyArg()
			}

			args[len(args)-1] = patch

			mock.ExpectExec(tt.query).
				WithArgs(args...).
				WillReturnResult(sqlmock.NewResult(0, 1))

			store := NewPostgreSQLHealthEventStore(db)
			require.NoError(t, store.UpdateHealthEventStatus(context.Background(), "event-1", tt.status))

			mock.ExpectClose()
			require.NoError(t, db.Close())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUpdateHealthEventStatus_DecisionOnlyUpdateKeepsOtherFields(t *testing.T) {
	tests := []struct {
		name   string
		status datastore.HealthEventStatus
	}{
		{
			name: "drain decision only",
			status: datastore.HealthEventStatus{
				DrainDecision: &datastore.DrainDecision{RuleSet: "slurm", Method: "Custom"},
			},
		},
		{
			name: "remediation decision only",
			status: datastore.HealthEventStatus{
				RemediationDecision: &datastore.RemediationDecision{Action: "REPLACE_VM"},
			},
		},
		{
			name:   "no field set",
			status: datastore.HealthEventStatus{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotQuery string

			matcher := sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
				gotQuery = actualSQL

				return nil
			})

			db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
			require.NoError(t, err)

			patch, err := statusDecisionsPatch(tt.status)
			require.NoError(t, err)

			mock.ExpectExec("").
				WithArgs(patch, "event-1").
				WillReturnResult(sqlmock.NewResult(0, 1))

			store := NewPostgreSQLHealthEventStore(db)
			require.NoError(t, store.UpdateHealthEventStatus(context.Background(), "event-1", tt.status))

			assert.Contains(t, gotQuery, `|| $1::jsonb`)

			for _, field := range []string{
				"node_quarantined", "nodequarantined", "quarantine_finish_timestamp",
				"user_pods_eviction", "userpodsevictionstatus", "drain_finish_timestamp",
				"fault_remediated", "faultremediated", "last_remediation_timestamp", "lastremediationtimestamp",
			} {
				assert.NotContains(t, gotQuery, field)
			}

			mock.ExpectClose()
			require.NoError(t, db.Close())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}
