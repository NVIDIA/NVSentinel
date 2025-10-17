// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/health/types"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	"github.com/stretchr/testify/assert"
)

// helper to create a basic AWS Health Event with the supplied status code
func newTestAWSEvent(status types.EventStatusCode, lastUpdated *time.Time) types.Event {
	start := time.Now().Add(2 * time.Hour).UTC()
	end := start.Add(1 * time.Hour)

	e := types.Event{
		Arn:            aws.String("arn:aws:health:us-east-1::event/EC2/TEST_EVENT"),
		Service:        aws.String("EC2"),
		EventTypeCode:  aws.String("AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"),
		EventScopeCode: types.EventScopeCodeAccountSpecific,
		StatusCode:     status,
		StartTime:      &start,
		EndTime:        &end,
	}
	// Only set LastUpdatedTime if a pointer is provided
	if lastUpdated != nil {
		e.LastUpdatedTime = lastUpdated
	}
	return e
}

// helper to create standard metadata used across tests
func newTestMetadata() EventMetadata {
	return EventMetadata{
		NodeName:   "test-node",
		InstanceId: "i-0123456789abcdef0",
		EntityArn:  "arn:aws:ec2:us-east-1:123456789012:instance/i-0123456789abcdef0",
		Action:     "RESTART_VM",
	}
}

func TestAWSNormalizer_UpcomingEvent(t *testing.T) {
	n := &AWSNormalizer{}
	testEvent := newTestAWSEvent(types.EventStatusCodeUpcoming, nil)
	meta := newTestMetadata()

	normalized, err := n.Normalize(testEvent, meta)
	assert.NoError(t, err)

	assert.Equal(t, meta.EntityArn, normalized.EventID)
	assert.Equal(t, meta.NodeName, normalized.NodeName)
	assert.Equal(t, meta.InstanceId, normalized.ResourceID)
	assert.Equal(t, "EC2", normalized.ResourceType)

	assert.Equal(t, model.TypeScheduled, normalized.MaintenanceType)
	assert.Equal(t, model.StatusDetected, normalized.Status)

	// Upcoming -> should not have actual times set
	assert.Nil(t, normalized.ActualStartTime)
	assert.Nil(t, normalized.ActualEndTime)

	// Scheduled times should match what we provided (within nanoseconds)
	assert.WithinDuration(t, *testEvent.StartTime, *normalized.ScheduledStartTime, time.Nanosecond)
	assert.WithinDuration(t, *testEvent.EndTime, *normalized.ScheduledEndTime, time.Nanosecond)
}

func TestAWSNormalizer_OpenEvent(t *testing.T) {
	n := &AWSNormalizer{}
	lastUpdated := time.Now().UTC()
	testEvent := newTestAWSEvent(types.EventStatusCodeOpen, &lastUpdated)
	meta := newTestMetadata()

	normalized, err := n.Normalize(testEvent, meta)
	assert.NoError(t, err)

	assert.Equal(t, model.StatusMaintenanceOngoing, normalized.Status)
	assert.NotNil(t, normalized.ActualStartTime)
	assert.Nil(t, normalized.ActualEndTime)
}

func TestAWSNormalizer_ClosedEvent(t *testing.T) {
	n := &AWSNormalizer{}
	lastUpdated := time.Now().UTC()
	testEvent := newTestAWSEvent(types.EventStatusCodeClosed, &lastUpdated)
	meta := newTestMetadata()

	normalized, err := n.Normalize(testEvent, meta)
	assert.NoError(t, err)

	assert.Equal(t, model.StatusMaintenanceComplete, normalized.Status)
	assert.NotNil(t, normalized.ActualEndTime)
	assert.Nil(t, normalized.ActualStartTime)
}

func TestAWSNormalizer_MissingMetadata(t *testing.T) {
	n := &AWSNormalizer{}
	testEvent := newTestAWSEvent(types.EventStatusCodeUpcoming, nil)

	_, err := n.Normalize(testEvent /* missing metadata */)
	assert.Error(t, err)
}
