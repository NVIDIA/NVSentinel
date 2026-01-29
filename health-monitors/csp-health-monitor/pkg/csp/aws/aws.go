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
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/health"
	"github.com/aws/aws-sdk-go-v2/service/health/types"
	"github.com/hashicorp/go-multierror"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/config"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/datastore"
	eventpkg "github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/event"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/metrics"
	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

const (
	INSTANCE_NETWORK_MAINTENANCE_SCHEDULED       = "AWS_EC2_INSTANCE_NETWORK_MAINTENANCE_SCHEDULED"
	INSTANCE_POWER_MAINTENANCE_SCHEDULED         = "AWS_EC2_INSTANCE_POWER_MAINTENANCE_SCHEDULED"
	INSTANCE_REBOOT_MAINTENANCE_SCHEDULED        = "AWS_EC2_INSTANCE_REBOOT_MAINTENANCE_SCHEDULED"
	MAINTENANCE_SCHEDULED                        = "AWS_EC2_MAINTENANCE_SCHEDULED"
	DEDICATED_HOST_MAINTENANCE_SCHEDULED         = "AWS_EC2_DEDICATED_HOST_MAINTENANCE_SCHEDULED"
	DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED = "AWS_EC2_DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED"
	DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED   = "AWS_EC2_DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED"
	ULTRASERVER_MAINTENANCE_INITIATED            = "AWS_EC2_ULTRASERVER_MAINTENANCE_INITIATED"
	ULTRASERVER_CAPACITY_REDUCED                 = "AWS_EC2_ULTRASERVER_CAPACITY_REDUCED"
	ULTRASERVER_MAINTENANCE_COMPLETED            = "AWS_EC2_ULTRASERVER_MAINTENANCE_COMPLETED"
)

var SupportedEventTypeCodes = map[string]string{
	INSTANCE_NETWORK_MAINTENANCE_SCHEDULED:       "",
	INSTANCE_POWER_MAINTENANCE_SCHEDULED:         "",
	INSTANCE_REBOOT_MAINTENANCE_SCHEDULED:        "",
	MAINTENANCE_SCHEDULED:                        "",
	DEDICATED_HOST_MAINTENANCE_SCHEDULED:         "",
	DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED: "",
	DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED:   "",
	ULTRASERVER_MAINTENANCE_INITIATED:            "",
	ULTRASERVER_CAPACITY_REDUCED:                 "",
	ULTRASERVER_MAINTENANCE_COMPLETED:            "",
}

var SupportedEventTypeCodesList = []string{
	INSTANCE_NETWORK_MAINTENANCE_SCHEDULED,
	INSTANCE_POWER_MAINTENANCE_SCHEDULED,
	INSTANCE_REBOOT_MAINTENANCE_SCHEDULED,
	MAINTENANCE_SCHEDULED,
	DEDICATED_HOST_MAINTENANCE_SCHEDULED,
	DEDICATED_HOST_NETWORK_MAINTENANCE_SCHEDULED,
	DEDICATED_HOST_POWER_MAINTENANCE_SCHEDULED,
	ULTRASERVER_MAINTENANCE_INITIATED,
	ULTRASERVER_CAPACITY_REDUCED,
	ULTRASERVER_MAINTENANCE_COMPLETED,
}

func isSupportedEventTypeCode(code string) bool {
	_, ok := SupportedEventTypeCodes[code]
	return ok
}

type healthClientInterface interface {
	DescribeEvents(
		ctx context.Context,
		params *health.DescribeEventsInput,
		optFns ...func(*health.Options),
	) (*health.DescribeEventsOutput, error)
	DescribeAffectedEntities(
		ctx context.Context,
		params *health.DescribeAffectedEntitiesInput,
		optFns ...func(*health.Options),
	) (*health.DescribeAffectedEntitiesOutput, error)
	DescribeEventDetails(
		ctx context.Context,
		params *health.DescribeEventDetailsInput,
		optFns ...func(*health.Options),
	) (*health.DescribeEventDetailsOutput, error)
}

type AWSClient struct {
	config         config.AWSConfig
	awsClient      healthClientInterface
	k8sClient      kubernetes.Interface
	normalizer     eventpkg.Normalizer
	clusterName    string
	kubeconfigPath string
	store          datastore.Store
	nodeInformer   *NodeInformer
}

func NewClient(
	ctx context.Context,
	cfg config.AWSConfig,
	clusterName string,
	kubeconfigPath string,
	store datastore.Store,
) (*AWSClient, error) {
	awsSDKConfig, err := awsConfig.LoadDefaultConfig(ctx, awsConfig.WithRegion(cfg.Region))
	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "aws_sdk_config_error").Inc()

		return nil, fmt.Errorf("failed to load AWS SDK config for region %s: %w",
			cfg.Region, err)
	}

	var healthAPIClient healthClientInterface

	if cfg.EndpointOverride != "" {
		slog.Info("AWS Client: Using endpoint override", "endpoint", cfg.EndpointOverride)

		healthAPIClient = health.NewFromConfig(awsSDKConfig, func(o *health.Options) {
			o.BaseEndpoint = aws.String(cfg.EndpointOverride)
		})
	} else {
		healthAPIClient = health.NewFromConfig(awsSDKConfig)
	}

	slog.Info("Successfully initialized AWS Health client", "region", cfg.Region)

	var k8sClient kubernetes.Interface

	var k8sRestConfig *rest.Config

	if kubeconfigPath != "" {
		slog.Info("AWS Client: Using kubeconfig from path", "path", kubeconfigPath)
		k8sRestConfig, err = clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	} else {
		slog.Info("AWS Client: KubeconfigPath not specified, attempting in-cluster config")

		k8sRestConfig, err = rest.InClusterConfig()
	}

	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "k8s_config_error").Inc()

		return nil, fmt.Errorf(
			"AWS client failed to initialize K8s config (kubeconfig: '%s'): %w",
			kubeconfigPath, err,
		)
	}

	k8sClient, err = kubernetes.NewForConfig(k8sRestConfig)
	if err != nil {
		metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "k8s_clientset_error").Inc()
		return nil, fmt.Errorf("AWS client failed to create K8s clientset: %w", err)
	}

	slog.Info("AWS Client: Kubernetes clientset initialized successfully.")

	nodeInformer, err := NewNodeInformer(k8sClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create node informer: %w", err)
	}

	nodeInformer.Start(ctx)

	slog.Info("AWS Client: Node informer started successfully")

	normalizer, err := eventpkg.GetNormalizer(model.CSPAWS)
	if err != nil {
		return nil, fmt.Errorf("failed to get AWS normalizer: %w", err)
	}

	return &AWSClient{
		config:         cfg,
		awsClient:      healthAPIClient,
		k8sClient:      k8sClient,
		normalizer:     normalizer,
		clusterName:    clusterName,
		kubeconfigPath: kubeconfigPath,
		store:          store,
		nodeInformer:   nodeInformer,
	}, nil
}

func (c *AWSClient) GetName() model.CSP {
	return model.CSPAWS
}

// StartMonitoring polls the AWS Health API periodically.
func (c *AWSClient) StartMonitoring(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	slog.Info("Starting AWS Health API polling",
		"intervalSeconds", c.config.PollingIntervalSeconds,
		"region", c.config.Region)

	ticker := time.NewTicker(time.Duration(c.config.PollingIntervalSeconds) * time.Second)
	defer ticker.Stop()

	lastEventProcessedTime := c.getInitialPollStartTime(ctx)
	slog.Debug("Starting first poll", "from", lastEventProcessedTime)

	if err := c.pollNewEvents(ctx, eventChan, lastEventProcessedTime); err != nil {
		slog.Error("Initial error polling AWS Health events", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("Context cancelled, AWS monitoring stopped")
			return ctx.Err()
		case <-ticker.C:
			var wg sync.WaitGroup

			wg.Add(2)

			go func() {
				defer wg.Done()

				if err := c.pollActiveEvents(ctx, eventChan); err != nil {
					metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "updating_active_events_status_error").Inc()
					slog.Error("Error refreshing active events status", "error", err)
				}
			}()

			go func() {
				defer wg.Done()

				pollStartTime := time.Now().UTC().Add(-time.Duration(c.config.PollingIntervalSeconds) * time.Second)
				if err := c.pollNewEvents(ctx, eventChan, pollStartTime); err != nil {
					metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "poll_events_error").Inc()
					slog.Error("Error polling AWS Health events", "error", err)
				}
			}()

			wg.Wait()
		}
	}
}

// getInitialPollStartTime determines the starting point for polling events.
// It prioritizes the last processed timestamp from the datastore, falling back
// to the current time.
func (c *AWSClient) getInitialPollStartTime(
	ctx context.Context,
) time.Time {
	defaultPollStartTime := time.Now().UTC().Add(-time.Duration(c.config.PollingIntervalSeconds) * time.Second)

	if c.store == nil {
		slog.Warn("Datastore client is nil for AWS monitor. Starting poll from current time.")

		return defaultPollStartTime
	}

	lastProcessedEventTS, found, errDb := c.store.GetLastProcessedEventTimestampByCSP(
		ctx,
		c.clusterName,
		model.CSPAWS,
		"AWS",
	)
	if errDb != nil {
		slog.Warn(
			"Failed to get last processed AWS event timestamp from datastore; Starting poll from current time",
			"error", errDb,
		)

		return defaultPollStartTime
	}

	if found && !lastProcessedEventTS.IsZero() {
		slog.Info(
			"Resuming poll...",
			"last processed timestamp", lastProcessedEventTS.Format(time.RFC3339Nano),
		)

		return lastProcessedEventTS
	}

	slog.Info("No previous AWS logs checkpoint found in datastore, starting poll from current time",
		"cluster", c.clusterName)

	return defaultPollStartTime
}

func (c *AWSClient) pollNewEvents(ctx context.Context,
	eventChan chan<- model.MaintenanceEvent,
	pollStartTime time.Time) error {
	pollStart := time.Now()

	defer func() {
		metrics.CSPPollingDuration.WithLabelValues(string(model.CSPAWS)).Observe(time.Since(pollStart).Seconds())
	}()

	slog.Debug("Polling AWS Health API")

	err := c.handleMaintenanceEvents(ctx, eventChan, pollStartTime)
	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "handle_maintenance_events_error").Inc()
		slog.Error("Error polling AWS Health events", "error", err)

		return fmt.Errorf("error polling AWS Health events: %w", err)
	}

	return nil
}

// handleMaintenanceEvents performs a single poll request to the AWS Health API.
func (c *AWSClient) handleMaintenanceEvents(
	ctx context.Context,
	eventChan chan<- model.MaintenanceEvent,
	pollStartTime time.Time,
) error {
	slog.Debug("AWS Poll: Checking all maintenance events", "asOf", pollStartTime)

	events, err := c.pollEventsAPI(ctx, pollStartTime)
	if err != nil {
		slog.Error("Error polling AWS Health events", "error", err)
		return err
	}

	if len(events) == 0 {
		slog.Debug("No AWS EC2 maintenance events found")
		return nil
	}

	eventArnsMap := make(map[string]types.Event)

	for _, event := range events {
		metrics.CSPEventsReceived.WithLabelValues(string(model.CSPAWS)).Inc()

		if event.EventTypeCode == nil {
			slog.Debug("Skipping event with nil EventTypeCode", "arn", aws.ToString(event.Arn))
			continue
		}

		if event.Arn == nil {
			slog.Debug("Skipping event with nil ARN")
			continue
		}

		if !isSupportedEventTypeCode(*event.EventTypeCode) {
			metrics.CSPEventsByTypeUnsupported.WithLabelValues(string(model.CSPAWS), *event.EventTypeCode).Inc()
			slog.Debug("Ignoring unsupported event type code",
				"typeCode", *event.EventTypeCode,
				"arn", aws.ToString(event.Arn))

			continue
		}

		slog.Debug("Processing maintenance event",
			"arn", aws.ToString(event.Arn),
			"typeCode", *event.EventTypeCode,
			"status", string(event.StatusCode))

		eventArnsMap[*event.Arn] = event
	}

	slog.Info("Found AWS maintenance scheduled events", "count", len(eventArnsMap))

	var wg sync.WaitGroup

	var mu sync.Mutex

	var errs *multierror.Error

	for eventID, event := range eventArnsMap {
		wg.Add(1)

		go func(eventID string, eventData types.Event) {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Panic recovered while processing AWS Health event",
						"eventID", eventID,
						"panic", r)
				}

				wg.Done()
			}()

			instanceIDs := c.nodeInformer.GetInstanceIDs()

			err := c.processAWSHealthEvent(ctx, eventID, eventData, instanceIDs, eventChan)
			if err != nil {
				slog.Error("Error processing AWS Health event",
					"eventID", eventID,
					"error", err)

				mu.Lock()

				errs = multierror.Append(errs, err)

				mu.Unlock()
			}
		}(eventID, event)
	}

	wg.Wait()

	return errs.ErrorOrNil()
}

func (c *AWSClient) processSingleEntityForEvent(
	ctx context.Context,
	eventArn string,
	evt types.Event,
	entity types.AffectedEntity,
	desc string,
	action pb.RecommendedAction,
	nodeMap map[string]string,
	eventChan chan<- model.MaintenanceEvent,
) error {
	if entity.EntityValue == nil {
		slog.Warn("Entity with nil EntityValue", "eventArn", eventArn)
		return fmt.Errorf("entity with nil EntityValue: %s", eventArn)
	}

	instanceID := *entity.EntityValue

	nodeName, ok := nodeMap[instanceID]
	if !ok {
		slog.Warn("Instance ID not found in node map", "instanceID", instanceID)
		return fmt.Errorf("instance ID not found in node map: %s", instanceID)
	}

	if entity.EventArn == nil || entity.EntityArn == nil {
		slog.Error("Affected entity doesn't have complete information",
			"instanceID", instanceID,
			"eventArn", entity.EventArn,
			"entityArn", entity.EntityArn)

		return fmt.Errorf("affected entity doesn't have complete information: %s", eventArn)
	}

	eventMetadata := eventpkg.EventMetadata{
		Event:            evt,
		NodeName:         nodeName,
		InstanceId:       instanceID,
		EntityArn:        aws.ToString(entity.EntityArn),
		ClusterName:      c.clusterName,
		Action:           action.String(),
		EventDescription: desc,
	}

	normalizedEvent, err := c.normalizer.Normalize(evt, eventMetadata)
	if err != nil {
		metrics.MainNormalizationErrors.WithLabelValues(string(model.CSPAWS)).Inc()
		slog.Error(
			"Error normalizing AWS event",
			"node", nodeName,
			"instanceID", instanceID,
			"eventArn", eventArn,
			"error", err,
		)

		return fmt.Errorf("error normalizing AWS event: %w", err)
	}

	metrics.MainEventsToNormalize.WithLabelValues(string(model.CSPAWS)).Inc()

	select {
	case eventChan <- *normalizedEvent:
		slog.Info("Dispatched maintenance event",
			"node", nodeName,
			"instanceID", instanceID,
			"eventArn", eventArn)
	case <-ctx.Done():
		slog.Warn("Context cancelled while sending event",
			"node", nodeName,
			"instanceID", instanceID,
			"eventArn", eventArn)

		return fmt.Errorf("context cancelled while sending event for node %s (instance %s, event %s)",
			nodeName,
			instanceID,
			eventArn,
		)
	}

	return nil
}

func (c *AWSClient) processAWSHealthEvent(
	ctx context.Context,
	eventArn string,
	evt types.Event,
	nodeMap map[string]string,
	eventChan chan<- model.MaintenanceEvent,
) error {
	slog.Debug("Processing AWS Health event", "arn", eventArn)

	var errs *multierror.Error

	desc := c.getEventDescription(ctx, evt)
	action := c.mapToValidAction(desc)

	affectedEntities, err := c.getAffectedEntities(ctx, eventArn)
	if err != nil {
		slog.Error("Error getting affected entities for event",
			"eventArn", eventArn,
			"error", err)

		return fmt.Errorf("error getting affected entities for event: %w", err)
	}

	for _, entity := range affectedEntities {
		err := c.processSingleEntityForEvent(ctx, eventArn, evt, entity, desc, action, nodeMap, eventChan)
		if err != nil {
			slog.Error("Error processing single entity for event",
				"eventArn", eventArn,
				"error", err)

			errs = multierror.Append(errs, err)
		}
	}

	return errs.ErrorOrNil()
}

func (c *AWSClient) getEventDescription(ctx context.Context, event types.Event) string {
	start := time.Now()
	detailedEvents, err := c.awsClient.DescribeEventDetails(ctx, &health.DescribeEventDetailsInput{
		EventArns: []string{*event.Arn},
	})

	metrics.CSPAPIDuration.WithLabelValues(string(model.CSPAWS),
		"describe_event_details").Observe(time.Since(start).Seconds())

	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "describe_event_details").Inc()
		slog.Error("Error getting event details for event", "eventArn", *event.Arn, "error", err)

		return ""
	}

	if len(detailedEvents.SuccessfulSet) == 0 {
		slog.Error("No event details found for event", "eventArn", *event.Arn)

		return ""
	}

	if detailedEvents.SuccessfulSet[0].EventDescription == nil {
		slog.Error("Event has nil EventDescription", "eventArn", *event.Arn)
		return ""
	}

	desc := detailedEvents.SuccessfulSet[0].EventDescription.LatestDescription
	if desc == nil {
		return ""
	}

	return *desc
}

// getAffectedEntities retrieves affected entities for a specific event
func (c *AWSClient) getAffectedEntities(
	ctx context.Context,
	eventARN string,
) ([]types.AffectedEntity, error) {
	slog.Debug("Fetching affected entities for event", "eventArn", eventARN)

	start := time.Now()
	detailedEvents, err := c.awsClient.DescribeAffectedEntities(ctx, &health.DescribeAffectedEntitiesInput{
		Filter: &types.EntityFilter{
			EventArns: []string{eventARN},
		},
	})

	metrics.CSPAPIDuration.WithLabelValues(string(model.CSPAWS), "describe_affected_entities").
		Observe(time.Since(start).Seconds())

	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "DescribeAffectedEntities").Inc()
		return nil, fmt.Errorf("error describing affected entities: %w", err)
	}

	if len(detailedEvents.Entities) == 0 {
		slog.Debug("No affected entities found for event", "eventARN", eventARN)
	}

	slog.Debug("Found affected entities for event",
		"count", len(detailedEvents.Entities),
		"eventARN", eventARN)

	return detailedEvents.Entities, nil
}

func (c *AWSClient) processActiveEvent(
	ctx context.Context,
	activeEvent model.MaintenanceEvent,
	eventChan chan<- model.MaintenanceEvent,
) error {
	awsEvent, awsStatus, err := c.checkStatusOfKnownEvents(ctx, activeEvent)
	if err != nil {
		return fmt.Errorf("checkStatusOfKnownEvents: %w", err)
	}

	if awsStatus == string(model.CSPStatusUnknown) {
		slog.Warn("AWS status is unknown for event",
			"eventArn", activeEvent.Metadata["eventArn"])

		err := c.store.UpdateEventStatus(ctx, activeEvent.EventID, model.StatusError)
		if err != nil {
			return fmt.Errorf("failed to update event status: %w", err)
		}

		return nil
	}

	slog.Info("Checking if AWS status is the same as the active event status",
		"eventArn", activeEvent.Metadata["eventArn"],
		"awsStatus", awsStatus,
		"activeEventCSPStatus", string(activeEvent.CSPStatus))

	if model.ProviderStatus(awsStatus) == activeEvent.CSPStatus {
		slog.Debug("No change in the status, skipping the event update",
			"awsStatus", awsStatus,
			"activeEventCSPStatus", string(activeEvent.CSPStatus))

		return nil
	}

	nodeName, instanceID, eventArn := activeEvent.NodeName, activeEvent.ResourceID, activeEvent.Metadata["eventArn"]
	eventMetadata := eventpkg.EventMetadata{
		Event:            awsEvent,
		NodeName:         nodeName,
		InstanceId:       instanceID,
		EntityArn:        activeEvent.EventID,
		Action:           activeEvent.RecommendedAction,
		EventDescription: activeEvent.Metadata["description"],
	}

	normalizedEvent, err := c.normalizer.Normalize(awsEvent, eventMetadata)
	if err != nil {
		metrics.MainNormalizationErrors.WithLabelValues(string(model.CSPAWS)).Inc()
		slog.Error(
			"Error normalizing AWS event",
			"nodeName", nodeName,
			"instanceID", instanceID,
			"eventArn", activeEvent.Metadata["eventArn"],
			"error", err,
		)

		return fmt.Errorf("error normalizing AWS event for node %s (instance %s, event %s): %w",
			nodeName,
			instanceID,
			activeEvent.Metadata["eventArn"],
			err,
		)
	}

	metrics.MainEventsToNormalize.WithLabelValues(string(model.CSPAWS)).Inc()

	select {
	case eventChan <- *normalizedEvent:
		slog.Info("Dispatched maintenance event",
			"node", nodeName,
			"instanceID", instanceID,
			"eventArn", eventArn)
	case <-ctx.Done():
		slog.Warn("Context cancelled while sending event",
			"node", nodeName,
			"instanceID", instanceID,
			"eventArn", eventArn)

		return fmt.Errorf("context cancelled while sending event for node %s (instance %s, event %s)",
			nodeName,
			instanceID,
			eventArn,
		)
	}

	return nil
}

// pollActiveEvents fetch events in non-final states from db
// and refresh their status against AWS Health.
func (c *AWSClient) pollActiveEvents(ctx context.Context, eventChan chan<- model.MaintenanceEvent) error {
	slog.Info("Polling active events")

	activeEvents, err := c.store.FindActiveEventsByStatuses(ctx, model.CSPAWS, []string{
		"upcoming",
		"open",
	})
	if err != nil {
		return fmt.Errorf("failed DB query for active events: %w", err)
	}

	if len(activeEvents) == 0 {
		slog.Debug("No active events in MaintenanceEvents collection")
		return nil
	}

	slog.Debug("Refreshing status for active events", "count", len(activeEvents))

	for _, activeEvent := range activeEvents {
		if err := c.processActiveEvent(ctx, activeEvent, eventChan); err != nil {
			slog.Error("Error processing active event", "error", err)
		}
	}

	return nil
}

func (c *AWSClient) pollEventsAPI(ctx context.Context, startTime time.Time) ([]types.Event, error) {
	pollStart := time.Now()
	filter := &types.EventFilter{
		Services:            []string{"EC2"},
		EventTypeCategories: []types.EventTypeCategory{types.EventTypeCategoryScheduledChange},
		EventTypeCodes:      SupportedEventTypeCodesList,
		Regions:             []string{c.config.Region},
		LastUpdatedTimes: []types.DateTimeRange{
			{
				From: aws.Time(startTime),
			},
		},
	}

	events, err := c.awsClient.DescribeEvents(ctx, &health.DescribeEventsInput{
		Filter: filter,
	})
	if err != nil {
		metrics.CSPAPIErrors.WithLabelValues(string(model.CSPAWS), "DescribeEvents_api_error").Inc()

		slog.Error("Error while fetching maintenance events", "error", err)

		return nil, fmt.Errorf("error while fetching maintenance events: %w", err)
	}

	if len(events.Events) > 0 {
		slog.Debug("Found scheduled maintenance events", "count", len(events.Events))
	} else {
		slog.Debug("No scheduled maintenance events found")
	}

	metrics.CSPAPIDuration.WithLabelValues(string(model.CSPAWS), "describe_events").
		Observe(time.Since(pollStart).Seconds())

	return events.Events, nil
}

func (c *AWSClient) checkStatusOfKnownEvents(ctx context.Context, activeEvent model.MaintenanceEvent) (
	types.Event, string, error) {
	awsEvents, err := c.awsClient.DescribeEvents(ctx, &health.DescribeEventsInput{
		Filter: &types.EventFilter{
			EventArns: []string{activeEvent.Metadata["eventArn"]},
		},
	})
	if err != nil {
		return types.Event{}, "", fmt.Errorf("error querying AWS for known events: %w", err)
	}

	if len(awsEvents.Events) == 0 {
		return types.Event{}, string(model.CSPStatusUnknown),
			fmt.Errorf("no events found for event %s", activeEvent.Metadata["eventArn"])
	}

	return awsEvents.Events[0], string(awsEvents.Events[0].StatusCode), nil
}

func (c *AWSClient) mapToValidAction(desc string) pb.RecommendedAction {
	lines := strings.Split(desc, "\n")

	for idx, line := range lines {
		if strings.Contains(strings.ToLower(line), "what do i need to do") {
			section := strings.ToLower(strings.Join(lines[idx:], " "))

			switch {
			case strings.Contains(section, "reset") && strings.Contains(section, "component"):
				return pb.RecommendedAction_RESTART_VM

			case strings.Contains(section, "stop and start") || strings.Contains(section, "reboot"):
				return pb.RecommendedAction_RESTART_VM

			case strings.Contains(section, "replace the instance") || strings.Contains(section, "launch a new"):
				return pb.RecommendedAction_REPLACE_VM

			default:
				metrics.CSPMonitorErrors.WithLabelValues(string(model.CSPAWS), "map_to_valid_action_error").Inc()
				slog.Debug("Found suggested action but unable to parse",
					"section", section)

				return pb.RecommendedAction_NONE
			}
		}
	}

	return pb.RecommendedAction_NONE
}
