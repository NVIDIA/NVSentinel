// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package aws

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/health"
	"github.com/aws/aws-sdk-go-v2/service/health/types"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const (
	// Test AWS configuration constants
	testRegion    = "us-east-1"
	testService   = "EC2"
	testAccountID = "123456789012"

	// Test node and instance constants
	testNodeName    = "test-node"
	testNodeName1   = "test-node-1"
	testNodeName2   = "test-node-2"
	testInstanceID  = "i-0123456789abcdef0"
	testInstanceID1 = "i-additional000000001"
	testInstanceID2 = "i-additional000000002"

	// Test event description
	testEventDescription = "What do I need to do?\nWe recommend that you reboot the instance which will restart the instance."
)

var (
	pollStartTime = time.Now().Add(-24 * time.Minute)
)

// MockAWSHealthClient is a mock for AWS Health client
type MockAWSHealthClient struct {
	mock.Mock
}

func (m *MockAWSHealthClient) DescribeEvents(
	ctx context.Context,
	params *health.DescribeEventsInput,
	optFns ...func(*health.Options),
) (*health.DescribeEventsOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*health.DescribeEventsOutput), args.Error(1)
}

func (m *MockAWSHealthClient) DescribeAffectedEntities(
	ctx context.Context,
	params *health.DescribeAffectedEntitiesInput,
	optFns ...func(*health.Options),
) (*health.DescribeAffectedEntitiesOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*health.DescribeAffectedEntitiesOutput), args.Error(1)
}

func (m *MockAWSHealthClient) DescribeEventDetails(
	ctx context.Context,
	params *health.DescribeEventDetailsInput,
	optFns ...func(*health.Options),
) (*health.DescribeEventDetailsOutput, error) {
	args := m.Called(ctx, params)
	return args.Get(0).(*health.DescribeEventDetailsOutput), args.Error(1)
}

func createTestClient(t *testing.T) (*AWSClient, *MockAWSHealthClient, *fake.Clientset) {
	mockAWSClient := new(MockAWSHealthClient)
	fakeK8sClient := fake.NewSimpleClientset()

	// Create a node with AWS provider ID
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNodeName,
		},
		Spec: v1.NodeSpec{
			ProviderID: "aws:///" + testRegion + "/" + testInstanceID,
		},
	}
	_, err := fakeK8sClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	assert.NoError(t, err)

	client := &AWSClient{
		config: config.AWSConfig{
			Region:                 testRegion,
			PollingIntervalSeconds: 60,
			Enabled:                true,
		},
		awsClient:  mockAWSClient,
		k8sClient:  fakeK8sClient,
		normalizer: &eventpkg.AWSNormalizer{},
	}

	return client, mockAWSClient, fakeK8sClient
}

func TestHandleMaintenanceEvents(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup test data
	startTime := time.Now().Add(24 * time.Hour)
	endTime := startTime.Add(2 * time.Hour)
	eventArn := fmt.Sprintf("arn:aws:health:%s::event/%s/AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED/test-event-1", testRegion, testService)
	entityArn := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID)

	// Setup AWS Health API mock responses
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:               aws.String(eventArn),
				Service:           aws.String(testService),
				EventTypeCode:     aws.String("AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"),
				EventTypeCategory: types.EventTypeCategoryScheduledChange,
				EventScopeCode:    types.EventScopeCodeAccountSpecific,
				StatusCode:        types.EventStatusCodeUpcoming,
				StartTime:         aws.Time(startTime),
				EndTime:           aws.Time(endTime),
				Region:            aws.String(testRegion),
			},
		},
	}, nil)

	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.Anything).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					EntityArn:       aws.String(entityArn),
					EntityValue:     aws.String(testInstanceID),
					EventArn:        aws.String(eventArn),
					StatusCode:      types.EntityStatusCodeImpaired,
					LastUpdatedTime: aws.Time(time.Now().Add(-20 * time.Second)),
				},
			},
		}, nil)

	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.Anything).Return(&health.DescribeEventDetailsOutput{
		SuccessfulSet: []types.EventDetails{
			{
				Event: &types.Event{
					Arn: aws.String(eventArn),
				},
				EventDescription: &types.EventDescription{
					LatestDescription: aws.String(testEventDescription),
				},
			},
		},
	}, nil)
	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Verify we received an event
	select {
	case event := <-eventChan:
		assert.Equal(t, entityArn, event.EventID)
		assert.Equal(t, testNodeName, event.NodeName)
		assert.Equal(t, testInstanceID, event.ResourceID)
		assert.Equal(t, model.StatusDetected, event.Status)
		assert.Equal(t, model.TypeScheduled, event.MaintenanceType)
		assert.Equal(t, testService, event.ResourceType)
		assert.Equal(t, startTime, *event.ScheduledStartTime)
		assert.Equal(t, endTime, *event.ScheduledEndTime)
		assert.Equal(t, pb.RecommendedAction_RESTART_VM.String(), event.RecommendedAction)
	default:
		t.Error("Expected to receive an event, but none was received")
	}
}

func TestNoMaintenanceEvents(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup AWS Health API mock with no events
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{},
	}, nil)

	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	mockAWSClient.AssertNotCalled(t, "DescribeAffectedEntities", mock.Anything, mock.Anything)
	mockAWSClient.AssertNotCalled(t, "DescribeEventDetails", mock.Anything, mock.Anything)
	// Verify no events were received
	select {
	case <-eventChan:
		t.Error("Did not expect to receive an event, but one was received")
	default:
		// This is expected, no events should be present
	}
}

// TestMultipleAffectedEntities tests that multiple instances affected by one event
// generate multiple maintenance events
func TestMultipleAffectedEntities(t *testing.T) {
	client, mockAWSClient, fakeK8sClient := createTestClient(t)

	// Create additional 3 nodes
	additionalNodes := []struct {
		name, instanceID string
	}{
		{testNodeName1, testInstanceID1},
		{testNodeName2, testInstanceID2},
	}

	for _, nodeData := range additionalNodes {
		node := &v1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeData.name,
			},
			Spec: v1.NodeSpec{
				ProviderID: "aws:///" + testRegion + "/" + nodeData.instanceID,
			},
		}
		_, err := fakeK8sClient.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
		assert.NoError(t, err)
	}

	// Setup test data
	startTime := time.Now().Add(24 * time.Hour)
	endTime := startTime.Add(2 * time.Hour)
	eventArn := fmt.Sprintf("arn:aws:health:%s::event/%s/AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED/test-event-1", testRegion, testService)
	entityArn1 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID)
	entityArn2 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID1)
	entityArn3 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID2)

	// Setup AWS Health API mock responses
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:               aws.String(eventArn),
				Service:           aws.String(testService),
				EventTypeCode:     aws.String("AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"),
				EventTypeCategory: types.EventTypeCategoryScheduledChange,
				EventScopeCode:    types.EventScopeCodeAccountSpecific,
				StatusCode:        types.EventStatusCodeUpcoming,
				StartTime:         aws.Time(startTime),
				EndTime:           aws.Time(endTime),
				Region:            aws.String(testRegion),
			},
		},
	}, nil)

	// Multiple affected instances for same event
	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.Anything).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					EntityArn:   aws.String(entityArn1),
					EntityValue: aws.String(testInstanceID),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					EntityArn:   aws.String(entityArn2),
					EntityValue: aws.String(testInstanceID1),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					EntityArn:   aws.String(entityArn3),
					EntityValue: aws.String(testInstanceID2),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
			},
		}, nil)

	// event with no details, default recommended action should be taken NONE
	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.Anything).Return(&health.DescribeEventDetailsOutput{
		SuccessfulSet: []types.EventDetails{},
	}, nil)

	// Setup test channel and instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID:  testNodeName,
		testInstanceID1: testNodeName1,
		testInstanceID2: testNodeName2,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Verify we received events for all affected instances
	receivedEvents := 0
	affectedNodes := make(map[string]bool)

	// Collect all events from channel (with timeout protection)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case event := <-eventChan:
			var expectedEntityArn string
			switch event.ResourceID {
			case testInstanceID:
				expectedEntityArn = entityArn1
			case testInstanceID1:
				expectedEntityArn = entityArn2
			case testInstanceID2:
				expectedEntityArn = entityArn3
			default:
				assert.Fail(t, "Unexpected resource ID: %s", event.ResourceID)
			}
			receivedEvents++
			affectedNodes[event.NodeName] = true
			assert.Equal(t, model.StatusDetected, event.Status)
			assert.Equal(t, expectedEntityArn, event.EventID)
		case <-timeout:
			// Break out of the loop after timeout
			goto checkResults
		default:
			if receivedEvents >= 3 {
				goto checkResults
			}
			time.Sleep(10 * time.Millisecond) // Small sleep to prevent CPU spin
		}
	}

checkResults:
	assert.Equal(t, 3, receivedEvents, "Should have received 3 maintenance events")
	assert.Equal(t, 3, len(affectedNodes), "Should have affected 3 distinct nodes")
}

// TestCompletedEvent tests that completed maintenance events generate recovery health events
func TestCompletedEvent(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup test data for a previously ongoing event that is now completed
	eventStartTime := time.Now().Add(-24 * time.Hour) // Started a day ago
	eventEndTime := time.Now().Add(-1 * time.Hour)    // Ended an hour ago
	lastUpdatedTime := time.Now().Add(-10 * time.Second)
	// Setup test data
	eventArn := fmt.Sprintf("arn:aws:health:%s::event/%s/AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED/test-event-1", testRegion, testService)

	// Setup AWS Health API mock responses
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:               aws.String(eventArn),
				Service:           aws.String(testService),
				EventTypeCode:     aws.String("AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"),
				EventTypeCategory: types.EventTypeCategoryScheduledChange,
				EventScopeCode:    types.EventScopeCodeAccountSpecific,
				StatusCode:        types.EventStatusCodeClosed,
				StartTime:         aws.Time(eventStartTime),
				EndTime:           aws.Time(eventEndTime),
				Region:            aws.String(testRegion),
				LastUpdatedTime:   &lastUpdatedTime,
			},
		},
	}, nil)

	entityArn1 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID)
	entityArn2 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID1)
	entityArn3 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID2)

	// Multiple affected instances for same event
	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.Anything).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					EntityArn:   aws.String(entityArn1),
					EntityValue: aws.String(testInstanceID),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					EntityArn:   aws.String(entityArn2),
					EntityValue: aws.String(testInstanceID1),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					EntityArn:   aws.String(entityArn3),
					EntityValue: aws.String(testInstanceID2),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
			},
		}, nil)

	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.Anything).Return(&health.DescribeEventDetailsOutput{
		SuccessfulSet: []types.EventDetails{
			{
				Event: &types.Event{
					Arn: aws.String(eventArn),
				},
				EventDescription: &types.EventDescription{
					LatestDescription: aws.String(testEventDescription),
				},
			},
		},
	}, nil)

	// Setup test channel and instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID:  testNodeName,
		testInstanceID1: testNodeName1,
		testInstanceID2: testNodeName2,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Verify we received a completed event
	select {
	case event := <-eventChan:
		var expectedEntityArn string
		var nodeName string
		switch event.ResourceID {
		case testInstanceID:
			expectedEntityArn = entityArn1
			nodeName = testNodeName
		case testInstanceID1:
			expectedEntityArn = entityArn2
			nodeName = testNodeName1
		case testInstanceID2:
			expectedEntityArn = entityArn3
			nodeName = testNodeName2
		default:
			assert.Fail(t, "Unexpected resource ID: %s", event.ResourceID)
		}
		assert.Equal(t, expectedEntityArn, event.EventID)
		assert.Equal(t, nodeName, event.NodeName)
		assert.Equal(t, model.StatusMaintenanceComplete, event.Status) // Should be maintenance complete status
		assert.NotNil(t, event.ActualEndTime, "Completed event should have ActualEndTime set")
	default:
		t.Error("Expected to receive a maintenance complete event, but none was received")
	}
}

// TestErrorScenario tests how the module handles API errors
func TestErrorScenario(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup mock to return error for DescribeEvents
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(
		(*health.DescribeEventsOutput)(nil),
		assert.AnError, // Mock error
	)

	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
	}

	// Call the function being tested - should not panic but return error
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.Error(t, err)

	mockAWSClient.AssertNotCalled(t, "DescribeAffectedEntities", mock.Anything, mock.Anything)
	mockAWSClient.AssertNotCalled(t, "DescribeEventDetails", mock.Anything, mock.Anything)
	// Verify no events were received
	select {
	case <-eventChan:
		t.Error("Did not expect to receive an event when API returned error")
	default:
		// This is expected, no events should be present
	}
}

// TestTimeWindowFiltering tests that events outside polling window are not processed
func TestTimeWindowFiltering(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup current time for test
	now := time.Now().UTC()

	// Set polling interval to 1 minute
	client.config.PollingIntervalSeconds = 60 // 1 minute

	pollStartTime := now.Add(-time.Duration(client.config.PollingIntervalSeconds) * time.Second)

	// Setup AWS Health API mock with an old event
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.MatchedBy(func(input *health.DescribeEventsInput) bool {
		// Verify time window is set correctly
		return len(input.Filter.LastUpdatedTimes) > 0 &&
			input.Filter.LastUpdatedTimes[0].From != nil &&
			now.Sub(*input.Filter.LastUpdatedTimes[0].From) <= time.Duration(61)*time.Second // Allow 1 second margin
	})).Return(&health.DescribeEventsOutput{
		// Return empty events list since time filtering happens in AWS API
		Events: []types.Event{},
	}, nil)

	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Verify no events were received (as our filter should exclude the old event)
	select {
	case <-eventChan:
		t.Error("Did not expect to receive event outside polling window")
	default:
		// This is expected, no events should be present
	}
}

// TestInstanceFiltering tests that only instances in the cluster are affected
func TestInstanceFiltering(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup test data
	startTime := time.Now().Add(24 * time.Hour)
	endTime := startTime.Add(2 * time.Hour)
	eventArn := fmt.Sprintf("arn:aws:health:%s::event/%s/AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED/test-event-4", testRegion, testService)
	entityArn1 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID)
	entityArn2 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID1)
	entityArn3 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID2)

	// Setup AWS Health API mock responses
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:            aws.String(eventArn),
				Service:        aws.String(testService),
				EventTypeCode:  aws.String("AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"),
				EventScopeCode: types.EventScopeCodeAccountSpecific,
				StatusCode:     types.EventStatusCodeUpcoming,
				StartTime:      aws.Time(startTime),
				EndTime:        aws.Time(endTime),
			},
		},
	}, nil)

	// Return affected entities including some not in our cluster
	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.Anything).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					// This entity is in our cluster
					EntityValue: aws.String(testInstanceID),
					EntityArn:   &entityArn1,
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					// These entities are not in our cluster
					EntityValue: aws.String("i-external000000001"),
					EntityArn:   &entityArn2,
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					EntityValue: aws.String("i-external000000002"),
					EntityArn:   &entityArn3,
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
			},
		}, nil)

	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.Anything).Return(&health.DescribeEventDetailsOutput{
		SuccessfulSet: []types.EventDetails{
			{
				Event: &types.Event{
					Arn: aws.String(eventArn),
				},
				EventDescription: &types.EventDescription{
					LatestDescription: aws.String(
						" What do I need to do?\n'We recommend that you reboot the instance which will restart the instance.",
					),
				},
			},
		},
	}, nil)

	// Setup test channel with our cluster's instance IDs only
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
		// External instances are deliberately not included
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Should receive exactly one event for our cluster instance
	receivedEvents := 0

	timeout := time.After(1 * time.Second)
	for {
		select {
		case event := <-eventChan:
			receivedEvents++
			// Verify it's for our cluster's instance
			assert.Equal(t, testInstanceID, event.ResourceID)
			assert.Equal(t, testNodeName, event.NodeName)
		case <-timeout:
			goto checkResults
		default:
			if receivedEvents >= 1 {
				goto checkResults
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

checkResults:
	assert.Equal(t, 1, receivedEvents, "Should receive event only for our cluster's instance")
}

// TestInvalidEntityData tests handling of invalid entity data
func TestInvalidEntityData(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup test data
	startTime := time.Now().Add(24 * time.Hour)
	endTime := startTime.Add(2 * time.Hour)
	eventArn := fmt.Sprintf("arn:aws:health:%s::event/%s/AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED/test-event-5", testRegion, testService)
	entityArn1 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID)

	// Setup AWS Health API mock responses
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:            aws.String(eventArn),
				Service:        aws.String(testService),
				EventTypeCode:  aws.String("AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"),
				EventScopeCode: types.EventScopeCodeAccountSpecific,
				StatusCode:     types.EventStatusCodeUpcoming,
				StartTime:      aws.Time(startTime),
				EndTime:        aws.Time(endTime),
			},
		},
	}, nil)

	// Return entities with nil values to test error handling
	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.Anything).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					// Missing EntityValue
					EntityValue: nil,
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					// Missing EventArn
					EntityValue: aws.String(testInstanceID),
					EventArn:    nil,
					StatusCode:  types.EntityStatusCodeImpaired,
				},
				{
					// Valid entity
					EntityValue: aws.String(testInstanceID),
					EntityArn:   aws.String(entityArn1),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
			},
		}, nil)

	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.Anything).Return(&health.DescribeEventDetailsOutput{
		SuccessfulSet: []types.EventDetails{
			{
				Event: &types.Event{
					Arn: aws.String(eventArn),
				},
				EventDescription: &types.EventDescription{
					LatestDescription: aws.String(
						" What do I need to do?\n'We recommend that you reboot the instance which will restart the instance.",
					),
				},
			},
		},
	}, nil)

	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
	}

	// Call the function - should handle nil values without panicking
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Should still receive event for the valid entity
	select {
	case event := <-eventChan:
		assert.Equal(t, testInstanceID, event.ResourceID)
	default:
		t.Error("Expected to receive an event for the valid entity, but none was received")
	}
}

// Test for instance-reboot event type
func TestInstanceRebootEvent(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup test data for an instance-reboot event
	startTime := time.Now().Add(24 * time.Hour)
	endTime := startTime.Add(2 * time.Hour)
	eventArn := fmt.Sprintf(
		"arn:aws:health:%s::event/%s/%s/test-event-reboot",
		testRegion, testService, INSTANCE_REBOOT_MAINTENANCE_SCHEDULED,
	)
	entityArn1 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceID)

	// Setup AWS Health API mock with an instance-reboot event
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:            aws.String(eventArn),
				Service:        aws.String(testService),
				EventTypeCode:  aws.String(INSTANCE_REBOOT_MAINTENANCE_SCHEDULED), // instance-reboot event
				EventScopeCode: types.EventScopeCodeAccountSpecific,
				StatusCode:     types.EventStatusCodeUpcoming,
				StartTime:      aws.Time(startTime),
				EndTime:        aws.Time(endTime),
				// Add metadata field to include the event-code
				EventTypeCategory: types.EventTypeCategoryScheduledChange,
			},
		},
	}, nil)

	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.Anything).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					EntityValue: aws.String(testInstanceID),
					EntityArn:   aws.String(entityArn1),
					EventArn:    aws.String(eventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
			},
		}, nil)

	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.Anything).Return(&health.DescribeEventDetailsOutput{
		SuccessfulSet: []types.EventDetails{
			{
				Event: &types.Event{
					Arn: aws.String(eventArn),
				},
				EventDescription: &types.EventDescription{
					LatestDescription: aws.String(
						" What do I need to do?\n'We recommend that you reboot the instance which will restart the instance.",
					),
				},
			},
		},
	}, nil)
	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID: testNodeName,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Verify we received a maintenance event with correct type
	select {
	case event := <-eventChan:
		assert.Equal(t, entityArn1, event.EventID)
		assert.Equal(t, testNodeName, event.NodeName)
		assert.Equal(t, testInstanceID, event.ResourceID)
		assert.Equal(t, model.StatusDetected, event.Status)
		assert.Contains(t, event.Metadata["eventTypeCode"], "INSTANCE_REBOOT")
		assert.Equal(t, pb.RecommendedAction_RESTART_VM.String(), event.RecommendedAction)
	default:
		t.Error("Expected to receive an instance reboot event, but none was received")
	}
}

// Test that ignores instance retirement events
func TestIgnoredEventTypes(t *testing.T) {
	client, mockAWSClient, _ := createTestClient(t)

	// Setup test data for events that should be ignored
	startTime := time.Now().Add(24 * time.Hour)
	endTime := startTime.Add(2 * time.Hour)
	testInstanceIDIgnored := "i-0123456789abcdef1"
	testNodeNameIgnored := "test-node1"
	entityArn1 := fmt.Sprintf("arn:aws:ec2:%s:%s:instance/%s", testRegion, testAccountID, testInstanceIDIgnored)

	// Create two events that should be ignored
	instanceStopEventArn := fmt.Sprintf(
		"arn:aws:health:%s::event/%s/%s/test-event-stop",
		testRegion, testService, "AWS_EC2_INSTANCE_STOP_SCHEDULED",
	)
	instanceMaintenanceEventArn := fmt.Sprintf(
		"arn:aws:health:%s::event/%s/%s/test-event-retire",
		testRegion, testService, MAINTENANCE_SCHEDULED,
	)

	// Setup AWS Health API mock with events to be ignored
	mockAWSClient.On("DescribeEvents", mock.Anything, mock.Anything).Return(&health.DescribeEventsOutput{
		Events: []types.Event{
			{
				Arn:               aws.String(instanceStopEventArn),
				Service:           aws.String(testService),
				EventTypeCode:     aws.String("AWS_EC2_INSTANCE_STOP_SCHEDULED"), // Should be ignored
				EventScopeCode:    types.EventScopeCodeAccountSpecific,
				StatusCode:        types.EventStatusCodeUpcoming,
				StartTime:         aws.Time(startTime),
				EndTime:           aws.Time(endTime),
				EventTypeCategory: types.EventTypeCategoryScheduledChange,
			},
			{
				Arn:               aws.String(instanceMaintenanceEventArn),
				Service:           aws.String(testService),
				EventTypeCode:     aws.String(MAINTENANCE_SCHEDULED), // Should not be ignored
				EventScopeCode:    types.EventScopeCodeAccountSpecific,
				StatusCode:        types.EventStatusCodeUpcoming,
				StartTime:         aws.Time(startTime),
				EndTime:           aws.Time(endTime),
				EventTypeCategory: types.EventTypeCategoryScheduledChange,
			},
		},
	}, nil)

	// Expect DescribeAffectedEntities to be invoked only for the maintenance event ARN.
	mockAWSClient.On("DescribeAffectedEntities", mock.Anything, mock.MatchedBy(func(input *health.DescribeAffectedEntitiesInput) bool {
		return len(input.Filter.EventArns) == 1 && input.Filter.EventArns[0] == instanceMaintenanceEventArn
	})).
		Return(&health.DescribeAffectedEntitiesOutput{
			Entities: []types.AffectedEntity{
				{
					EntityValue: aws.String(testInstanceIDIgnored),
					EntityArn:   aws.String(entityArn1),
					EventArn:    aws.String(instanceMaintenanceEventArn),
					StatusCode:  types.EntityStatusCodeImpaired,
				},
			},
		}, nil)

	mockAWSClient.On("DescribeEventDetails", mock.Anything, mock.MatchedBy(func(input *health.DescribeEventDetailsInput) bool {
		return len(input.EventArns) == 1 && input.EventArns[0] == instanceMaintenanceEventArn
	})).
		Return(&health.DescribeEventDetailsOutput{
			SuccessfulSet: []types.EventDetails{
				{
					Event: &types.Event{
						Arn: aws.String(instanceMaintenanceEventArn),
					},
					EventDescription: &types.EventDescription{
						LatestDescription: aws.String(" What do I need to do?\n'We recommend that you reboot the instance which will restart the instance."),
					},
				},
			},
		}, nil)

	// Setup test channel and test instance IDs
	eventChan := make(chan model.MaintenanceEvent, 10)
	instanceIDs := map[string]string{
		testInstanceID:        testNodeName,
		testInstanceIDIgnored: testNodeNameIgnored,
	}

	// Call the function being tested
	err := client.handleMaintenanceEvents(context.Background(), instanceIDs, eventChan, pollStartTime)
	assert.NoError(t, err)

	// Verify no events were received (as these should be filtered out)
	select {
	case event := <-eventChan:
		assert.Equal(t, entityArn1, event.EventID)
		assert.Equal(t, testNodeNameIgnored, event.NodeName)
		assert.Equal(t, testInstanceIDIgnored, event.ResourceID)
		assert.Equal(t, model.StatusDetected, event.Status)
	default:
		// This is expected, no events should be present
	}
}
