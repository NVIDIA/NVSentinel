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

package event

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/akamai"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

var (
	akamaiWhen     = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	akamaiStarted  = akamaiWhen.Add(time.Minute)
	akamaiComplete = akamaiWhen.Add(30 * time.Minute)
	akamaiContext  = AkamaiEventContext{NodeName: "lke123456-gpu-abcde", ClusterName: "cluster-a"}
)

func ts(t time.Time) *akamai.Timestamp { return &akamai.Timestamp{Time: t} }

func akamaiEvent(status string) akamai.Maintenance {
	return akamai.Maintenance{
		ID:                   7,
		Entity:               akamai.MaintenanceEntity{ID: 11111111, Type: akamai.MaintenanceEntityLinode},
		Type:                 "power_off_on",
		Status:               status,
		Reason:               "Host upgrade",
		Description:          akamai.MaintenanceDescriptionScheduled,
		Source:               "platform",
		MaintenancePolicySet: "linode/power_off_on",
		When:                 ts(akamaiWhen),
	}
}

func TestAkamaiNormalizer_Normalize(t *testing.T) {
	got, err := (&AkamaiNormalizer{}).Normalize(akamaiEvent("scheduled"), akamaiContext)
	require.NoError(t, err)

	assert.Equal(t, "akamai-7", got.EventID)
	assert.Equal(t, model.CSPAkamai, got.CSP)
	assert.Equal(t, "cluster-a", got.ClusterName)
	assert.Equal(t, "akamai_instance", got.ResourceType)
	assert.Equal(t, "11111111", got.ResourceID)
	assert.Equal(t, "lke123456-gpu-abcde", got.NodeName)
	assert.Equal(t, model.TypeScheduled, got.MaintenanceType)
	assert.Equal(t, model.StatusDetected, got.Status)
	assert.Equal(t, model.ProviderStatus("scheduled"), got.CSPStatus)
	require.NotNil(t, got.ScheduledStartTime)
	assert.True(t, got.ScheduledStartTime.Equal(akamaiWhen))
	assert.Nil(t, got.ScheduledEndTime, "the API reports no end time")
	assert.Equal(t, "NONE", got.RecommendedAction)
	assert.Equal(t, "power_off_on", got.Metadata["maintenanceType"])
	assert.Equal(t, "linode/power_off_on", got.Metadata["policy"])
	assert.Equal(t, "scheduled", got.Metadata["urgency"])
}

// TestAkamaiNormalizer_AllTypesAreDisruptive pins that no maintenance type is
// filtered out: a GPU node cannot live-migrate with its workload, so even
// live_migration must still produce an event that quarantines the node.
func TestAkamaiNormalizer_AllTypesAreDisruptive(t *testing.T) {
	for _, typ := range []string{"reboot", "cold_migration", "live_migration", "migrate", "power_off_on", "unknown_new"} {
		raw := akamaiEvent("scheduled")
		raw.Type = typ

		got, err := (&AkamaiNormalizer{}).Normalize(raw, akamaiContext)
		require.NoError(t, err, typ)
		assert.Equal(t, model.StatusDetected, got.Status, typ)
		assert.NotNil(t, got.ScheduledStartTime, typ)
	}
}

func TestAkamaiNormalizer_Emergency(t *testing.T) {
	raw := akamaiEvent("pending")
	raw.Description = akamai.MaintenanceDescriptionEmergency

	got, err := (&AkamaiNormalizer{}).Normalize(raw, akamaiContext)
	require.NoError(t, err)

	assert.Equal(t, model.TypeUnscheduled, got.MaintenanceType)
	assert.Equal(t, model.MetadataUrgencyEmergency, got.Metadata["urgency"])
	assert.Nil(t, got.ScheduledStartTime, "emergency events skip the trigger window")
	assert.Equal(t, model.StatusDetected, got.Status)
}

func TestAkamaiNormalizer_StartTimeFallsBackToNotBefore(t *testing.T) {
	raw := akamaiEvent("scheduled")
	raw.When = nil
	raw.NotBefore = ts(akamaiWhen)

	got, err := (&AkamaiNormalizer{}).Normalize(raw, akamaiContext)
	require.NoError(t, err)
	require.NotNil(t, got.ScheduledStartTime)
	assert.True(t, got.ScheduledStartTime.Equal(akamaiWhen))
}

func TestMapAkamaiStatus(t *testing.T) {
	tests := []struct {
		name      string
		raw       akamai.Maintenance
		want      model.InternalStatus
		wantStart *time.Time
		wantEnd   bool
		endIsNow  bool
	}{
		{name: "pending", raw: akamaiEvent("pending"), want: model.StatusDetected},
		{name: "scheduled", raw: akamaiEvent("scheduled"), want: model.StatusDetected},
		{name: "unknown", raw: akamaiEvent("mystery"), want: model.StatusDetected},
		{name: "canceled", raw: akamaiEvent("canceled"), want: model.StatusCancelled},
		{
			name: "started uses start_time",
			raw: func() akamai.Maintenance {
				e := akamaiEvent("started")
				e.StartTime = ts(akamaiStarted)

				return e
			}(),
			want:      model.StatusMaintenanceOngoing,
			wantStart: &akamaiStarted,
		},
		{name: "in-progress without start_time", raw: akamaiEvent("in-progress"), want: model.StatusMaintenanceOngoing},
		{
			name: "completed uses complete_time",
			raw: func() akamai.Maintenance {
				e := akamaiEvent("completed")
				e.CompleteTime = ts(akamaiComplete)

				return e
			}(),
			want:    model.StatusMaintenanceComplete,
			wantEnd: true,
		},
		{name: "completed without complete_time", raw: akamaiEvent("completed"), want: model.StatusMaintenanceComplete,
			wantEnd: true, endIsNow: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, start, end := mapAkamaiStatus(tt.raw)
			assert.Equal(t, tt.want, status)

			if tt.wantStart != nil {
				require.NotNil(t, start)
				assert.True(t, start.Equal(*tt.wantStart))
			}

			if tt.want == model.StatusMaintenanceOngoing {
				assert.NotNil(t, start, "an ongoing event always has a start time")
			}

			if !tt.wantEnd {
				assert.Nil(t, end)

				return
			}

			// The healthy trigger needs an end time to fire at all.
			require.NotNil(t, end)

			if tt.endIsNow {
				assert.WithinDuration(t, time.Now(), *end, time.Minute)
			} else {
				assert.True(t, end.Equal(akamaiComplete))
			}
		})
	}
}

// TestAkamaiFingerprint pins that the dedup value changes whenever something
// the normalizer reads changes, and only then. A fingerprint that never changed
// would hide a cancellation or reschedule; one that always changed would reset
// QUARANTINE_TRIGGERED to DETECTED on every poll.
func TestAkamaiFingerprint(t *testing.T) {
	base := akamaiEvent("scheduled")
	assert.Equal(t, akamaiFingerprint(base), akamaiFingerprint(akamaiEvent("scheduled")))

	changes := map[string]func(*akamai.Maintenance){
		"status":      func(m *akamai.Maintenance) { m.Status = "canceled" },
		"type":        func(m *akamai.Maintenance) { m.Type = "reboot" },
		"description": func(m *akamai.Maintenance) { m.Description = "emergency" },
		"when":        func(m *akamai.Maintenance) { m.When = ts(akamaiWhen.Add(time.Hour)) },
		"not_before":  func(m *akamai.Maintenance) { m.NotBefore = ts(akamaiWhen) },
		"start_time":  func(m *akamai.Maintenance) { m.StartTime = ts(akamaiStarted) },
		"complete":    func(m *akamai.Maintenance) { m.CompleteTime = ts(akamaiComplete) },
	}

	for name, mutate := range changes {
		changed := akamaiEvent("scheduled")
		mutate(&changed)
		assert.NotEqual(t, akamaiFingerprint(base), akamaiFingerprint(changed), name)
	}

	got, err := (&AkamaiNormalizer{}).Normalize(base, akamaiContext)
	require.NoError(t, err)
	assert.Equal(t, akamaiFingerprint(base), got.Metadata[model.ProviderLastUpdatedKey])
}

func TestAkamaiNormalizer_Errors(t *testing.T) {
	n := &AkamaiNormalizer{}

	_, err := n.Normalize("not a maintenance", akamaiContext)
	require.Error(t, err)

	_, err = n.Normalize(akamaiEvent("scheduled"))
	require.Error(t, err)

	_, err = n.Normalize(akamaiEvent("scheduled"), "wrong context type")
	require.Error(t, err)

	zero := akamaiEvent("scheduled")
	zero.ID = 0

	_, err = n.Normalize(zero, akamaiContext)
	require.Error(t, err)
}

func TestGetNormalizer_Akamai(t *testing.T) {
	n, err := GetNormalizer(model.CSPAkamai)
	require.NoError(t, err)
	assert.IsType(t, &AkamaiNormalizer{}, n)
}
